package collect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

// userNotesFixtureServer serves the three xhs-user-notes fixture pages keyed
// by the cursor query param (empty cursor = first page) and records the
// cursor chain it received.
func userNotesFixtureServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	dir := fixturesDirForTest(t)
	var mu sync.Mutex
	var cursors []string
	pages := map[string]string{}
	prev := ""
	for i := 1; i <= 3; i++ {
		raw, err := os.ReadFile(filepath.Join(dir, "xhs-user-notes."+string(rune('0'+i))+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Data struct {
				Cursor string `json:"cursor"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		pages[prev] = body
		prev = doc.Data.Cursor
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := r.URL.Query().Get("cursor")
		mu.Lock()
		cursors = append(cursors, cur)
		body, ok := pages[cur]
		mu.Unlock()
		if !ok {
			t.Errorf("unexpected cursor %q", cur)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return srv, &cursors
}

// TestXHSUserNotesContractFields: the xhs-user-notes contract loads and
// declares the user-notes shape (W2-C2 AC-1): user_posted endpoint,
// web_session cookie, data.notes binding, cursor paging chain and the four
// stat bindings riding the item path.
func TestXHSUserNotesContractFields(t *testing.T) {
	all := contracts.NewRegistry()
	if err := contracts.LoadDir(all, testkit.ContractsDir(t, 2)); err != nil {
		t.Fatal(err)
	}
	c, ok := all.Get("xhs-user-notes")
	if !ok {
		t.Fatal("xhs-user-notes contract not registered")
	}
	if c.Transport.Path != "/api/sns/web/v1/user_posted" {
		t.Fatalf("path = %q", c.Transport.Path)
	}
	if len(c.Cookie.Required) != 1 || c.Cookie.Required[0] != "web_session" {
		t.Fatalf("cookie.required = %v, want [web_session]", c.Cookie.Required)
	}
	if c.Binding.Items != "$.data.notes" {
		t.Fatalf("binding.items = %q", c.Binding.Items)
	}
	if c.Paging.CursorParam != "cursor" || c.Paging.NextCursorPath != "$.data.cursor" || c.Paging.HasMorePath != "$.data.has_more" {
		t.Fatalf("paging = %+v", c.Paging)
	}
	for _, f := range []string{"stats.digg", "stats.comment", "stats.collect", "stats.share", "create_time", "media_type", "id", "desc", "user", "extra.images"} {
		if c.Binding.Fields[f] == "" {
			t.Fatalf("binding field %q missing", f)
		}
	}
	for _, banned := range []string{"verifyFp", "fp", "verify_fp"} {
		if _, ok := c.Transport.Query[banned]; ok {
			t.Fatalf("contract query hardcodes fingerprint param %q", banned)
		}
	}
}

func xhsUserNotesEngine(t *testing.T, srv *httptest.Server) (*Engine, *contracts.Contract) {
	t.Helper()
	reg := testkit.RemapContracts(t, testkit.ContractsDir(t, 2), srv, "xhs-user-notes")
	eng := New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Obs:      obs.NewCounterMap(),
		Cookies:  map[string]string{"xhs": "web_session=test-session"},
	})
	c, _ := reg.Get("xhs-user-notes")
	return eng, c
}

// TestXHSUserNotesPagination: fetchPages walks the cursor chain through all
// three fixture pages and stops on the terminal has_more=false (W2-C2 AC-2).
func TestXHSUserNotesPagination(t *testing.T) {
	srv, cursorsPtr := userNotesFixtureServer(t)
	defer srv.Close()
	eng, _ := xhsUserNotesEngine(t, srv)
	recs, next, err := eng.fetchPages(context.Background(), "xhs-user-notes",
		map[string]string{"user_id": "xhs-creator-0001"}, nil, model.Cursor{}, 0)
	if err != nil {
		t.Fatalf("fetchPages: %v", err)
	}
	if len(recs) != 6 {
		t.Fatalf("records = %d, want 6 (2+2+2 fixture notes)", len(recs))
	}
	cursors := *cursorsPtr
	if len(cursors) != 3 {
		t.Fatalf("pages fetched = %d, want 3 (cursors %v)", len(cursors), cursors)
	}
	if cursors[0] != "" {
		t.Fatalf("first cursor = %q, want empty (xhs first page)", cursors[0])
	}
	if cursors[1] == "" || cursors[2] == "" || cursors[1] == cursors[2] {
		t.Fatalf("cursor chain must advance: %v", cursors)
	}
	if next.HasMore {
		t.Fatal("terminal page must report has_more=false")
	}
}

// TestXHSUserNotesStatsBinding: every bound note carries the four stat
// counts, create_time and media_type image|video; image notes expose their
// picture list through Extra (W2-C2 AC-3/AC-5).
func TestXHSUserNotesStatsBinding(t *testing.T) {
	srv, _ := userNotesFixtureServer(t)
	defer srv.Close()
	eng, c := xhsUserNotesEngine(t, srv)
	recs, _, err := eng.fetchPages(context.Background(), "xhs-user-notes",
		map[string]string{"user_id": "xhs-creator-0001"}, nil, model.Cursor{}, 0)
	if err != nil {
		t.Fatalf("fetchPages: %v", err)
	}
	sawImage, sawVideo := false, false
	for _, r := range recs {
		it := bindItem(c, r)
		if it.ID == "" || it.Desc == "" || it.CreateTime == 0 {
			t.Fatalf("note %+v missing id/desc/create_time", it)
		}
		if it.Stats.Digg == 0 || it.Stats.Comment == 0 || it.Stats.Collect == 0 || it.Stats.Share == 0 {
			t.Fatalf("note %s missing stats: %+v", it.ID, it.Stats)
		}
		if it.Author.UID == "" || it.Author.Nickname == "" {
			t.Fatalf("note %s missing author", it.ID)
		}
		switch it.MediaType {
		case "video":
			sawVideo = true
		case "image":
			sawImage = true
			imgs, ok := it.Extra["images"].([]any)
			if !ok || len(imgs) == 0 {
				t.Fatalf("image note %s must expose images via extra.images", it.ID)
			}
		default:
			t.Fatalf("note %s media_type = %q, want video|image", it.ID, it.MediaType)
		}
	}
	if !sawVideo || !sawImage {
		t.Fatalf("fixtures must exercise both media types (video=%v image=%v)", sawVideo, sawImage)
	}
}
