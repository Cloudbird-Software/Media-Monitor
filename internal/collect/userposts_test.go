package collect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

// fixturesDirForTest resolves the adapt/fixtures dir from this package.
func fixturesDirForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..", "adapt", "fixtures")
}

// TestDouyinUserPostsContractFields: the douyin-user-posts contract loads and
// declares the user-posts shape end to end — endpoint, signing, cookie,
// items binding and max_cursor paging (W2-C1 AC-1/AC-5).
func TestDouyinUserPostsContractFields(t *testing.T) {
	all := contracts.NewRegistry()
	if err := contracts.LoadDir(all, testkit.ContractsDir(t, 2)); err != nil {
		t.Fatal(err)
	}
	c, ok := all.Get("douyin-user-posts")
	if !ok {
		t.Fatal("douyin-user-posts contract not registered")
	}
	if c.Transport.Path != "/aweme/v1/web/aweme/post/" {
		t.Fatalf("path = %q, want /aweme/v1/web/aweme/post/", c.Transport.Path)
	}
	if len(c.Signature.Required) != 1 || c.Signature.Required[0] != "a_bogus" {
		t.Fatalf("signature.required = %v, want [a_bogus]", c.Signature.Required)
	}
	if len(c.Cookie.Required) != 1 || c.Cookie.Required[0] != "ttwid" {
		t.Fatalf("cookie.required = %v, want [ttwid]", c.Cookie.Required)
	}
	if c.Binding.Items != "$.aweme_list" {
		t.Fatalf("binding.items = %q, want $.aweme_list", c.Binding.Items)
	}
	if c.Paging.CursorParam != "max_cursor" || c.Paging.NextCursorPath != "$.max_cursor" || c.Paging.HasMorePath != "$.has_more" {
		t.Fatalf("paging = %+v, want max_cursor chain", c.Paging)
	}
	// Fingerprint params must never be hardcoded into the contract query
	// (MediaCrawler #895 lesson: verifyFp/fp come from the account/UA pool).
	for _, banned := range []string{"verifyFp", "fp", "verify_fp", "msToken"} {
		if _, ok := c.Transport.Query[banned]; ok {
			t.Fatalf("contract query hardcodes fingerprint param %q", banned)
		}
	}
}

// userPostsFixtureServer serves the three douyin-user-posts fixture pages by
// max_cursor position and records the cursor positions it received.
func userPostsFixtureServer(t *testing.T) (*httptest.Server, *[]int64) {
	t.Helper()
	dir := fixturesDirForTest(t)
	var mu sync.Mutex
	var cursors []int64
	mu.Lock()
	loadPage := func(i int) string {
		raw, err := os.ReadFile(filepath.Join(dir, "douyin-user-posts."+strconv.Itoa(i)+".json"))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	page1 := loadPage(1)
	pages := map[int64]string{0: page1}
	var doc1 map[string]any
	if err := json.Unmarshal([]byte(page1), &doc1); err != nil {
		t.Fatal(err)
	}
	c1, _ := doc1["max_cursor"].(float64)
	page2 := loadPage(2)
	var doc2 map[string]any
	if err := json.Unmarshal([]byte(page2), &doc2); err != nil {
		t.Fatal(err)
	}
	c2, _ := doc2["max_cursor"].(float64)
	pages[int64(c1)] = page2
	pages[int64(c2)] = loadPage(3)
	mu.Unlock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur, _ := strconv.ParseInt(r.URL.Query().Get("max_cursor"), 10, 64)
		mu.Lock()
		cursors = append(cursors, cur)
		body, ok := pages[cur]
		mu.Unlock()
		if !ok {
			t.Errorf("unexpected max_cursor %d", cur)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return srv, &cursors
}

// TestDouyinUserPostsPagination: fetchPages driven by the three bundled
// fixture pages walks the max_cursor chain to the terminal page (W2-C1 AC-2):
// paging depth >= 3, the cursor strictly advances each page, and the third
// page's has_more=false terminates the walk.
func TestDouyinUserPostsPagination(t *testing.T) {
	srv, cursorsPtr := userPostsFixtureServer(t)
	defer srv.Close()
	reg := testkit.RemapContracts(t, testkit.ContractsDir(t, 2), srv, "douyin-user-posts")
	eng := New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Obs:      obs.NewCounterMap(),
		Signers: map[string]httpclient.Signer{"douyin": httpclient.StaticSigner{
			Fn: func(_ context.Context, _ string, _ string, _ map[string]string) (map[string]string, error) {
				return map[string]string{"a_bogus": "test-bogus"}, nil
			},
		}},
		Cookies: map[string]string{"douyin": "ttwid=test-ttwid"},
	})
	recs, next, err := eng.fetchPages(context.Background(), "douyin-user-posts",
		map[string]string{"sec_user_id": "MS4wLjABAAAA-example-creator-0000000001"}, nil, model.Cursor{}, 0)
	if err != nil {
		t.Fatalf("fetchPages: %v", err)
	}
	if len(recs) != 6 {
		t.Fatalf("records = %d, want 6 (2+2+2 fixture items)", len(recs))
	}
	cursors := *cursorsPtr
	if len(cursors) != 3 {
		t.Fatalf("pages fetched = %d, want 3", len(cursors))
	}
	if cursors[0] != 0 {
		t.Fatalf("first cursor = %d, want 0", cursors[0])
	}
	// max_cursor is the oldest item's timestamp (ms): each page strictly
	// moves further into the past — monotonic progression for a
	// newest-first history walk.
	for i := 2; i < len(cursors); i++ {
		if cursors[i] >= cursors[i-1] {
			t.Fatalf("cursor not advancing into the past: %v", cursors)
		}
	}
	if next.HasMore {
		t.Fatal("terminal page must report has_more=false")
	}
}

// TestDouyinUserPostsStatsBinding: every bound item carries the four stat
// counts plus create_time, media_type and an author summary (W2-C1 AC-3);
// play_count, when present, rides along via the extra binding.
func TestDouyinUserPostsStatsBinding(t *testing.T) {
	srv, _ := userPostsFixtureServer(t)
	defer srv.Close()
	reg := testkit.RemapContracts(t, testkit.ContractsDir(t, 2), srv, "douyin-user-posts")
	c, _ := reg.Get("douyin-user-posts")
	eng := New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Obs:      obs.NewCounterMap(),
		Signers: map[string]httpclient.Signer{"douyin": httpclient.StaticSigner{
			Fn: func(_ context.Context, _ string, _ string, _ map[string]string) (map[string]string, error) {
				return map[string]string{"a_bogus": "test-bogus"}, nil
			},
		}},
		Cookies: map[string]string{"douyin": "ttwid=test-ttwid"},
	})
	recs, _, err := eng.fetchPages(context.Background(), "douyin-user-posts",
		map[string]string{"sec_user_id": "MS4wLjABAAAA-example-creator-0000000001"}, nil, model.Cursor{}, 0)
	if err != nil {
		t.Fatalf("fetchPages: %v", err)
	}
	sawImage, sawVideo := false, false
	for _, r := range recs {
		it := bindItem(c, r)
		if it.ID == "" || it.Desc == "" || it.CreateTime == 0 {
			t.Fatalf("item %+v missing id/desc/create_time", it)
		}
		if it.Stats.Digg == 0 || it.Stats.Comment == 0 || it.Stats.Collect == 0 || it.Stats.Share == 0 {
			t.Fatalf("item %s missing stats: %+v", it.ID, it.Stats)
		}
		if it.Author.SecUID == "" || it.Author.Nickname == "" {
			t.Fatalf("item %s missing author summary", it.ID)
		}
		switch it.MediaType {
		case "video":
			sawVideo = true
		case "image":
			sawImage = true
		default:
			t.Fatalf("item %s media_type = %q, want video|image", it.ID, it.MediaType)
		}
		if it.Extra["play"] == nil {
			t.Fatalf("item %s: play_count present in fixture but not bound to extra.play", it.ID)
		}
	}
	if !sawVideo || !sawImage {
		t.Fatalf("fixtures must exercise both media types (video=%v image=%v)", sawVideo, sawImage)
	}
}
