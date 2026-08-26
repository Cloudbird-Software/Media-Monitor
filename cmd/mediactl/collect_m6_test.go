package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
)

// fakePlatform is an httptest twin of the platform endpoints the M6 collect
// contracts point at. It records request headers/query for assertions.
type fakePlatform struct {
	srv *httptest.Server
	mu  sync.Mutex
	// last request observations per path.
	cookie   map[string]string
	ua       map[string]string
	queryVal map[string]string
}

func newFakePlatform(t *testing.T) *fakePlatform {
	t.Helper()
	fp := &fakePlatform{cookie: map[string]string{}, ua: map[string]string{}, queryVal: map[string]string{}}
	fp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.mu.Lock()
		fp.cookie[r.URL.Path] = r.Header.Get("Cookie")
		fp.ua[r.URL.Path] = r.Header.Get("User-Agent")
		fp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search":
			fp.mu.Lock()
			fp.queryVal["keyword"] = r.URL.Query().Get("keyword")
			fp.mu.Unlock()
			_, _ = fmt.Fprint(w, `{"items":[{"aweme_id":"it1"}]}`)
		case "/detail":
			fmt.Fprintf(w, `{"aweme_detail":{"aweme_id":"AID1","video":{"play_addr":{"url_list":[%q]},"cover":{"url_list":["https://cover.example/c.jpg"]}}}}`, fp.srv.URL+"/video.mp4")
		case "/video.mp4":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = fmt.Fprint(w, "fake-video-bytes")
		case "/collects":
			if r.URL.Query().Get("cursor") == "2" {
				_, _ = fmt.Fprint(w, `{"collects":[{"collects_id":"f2"}],"has_more":false,"cursor":"3"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"collects":[{"collects_id":"f1"}],"has_more":true,"cursor":"2"}`)
		case "/collects-videos":
			fp.mu.Lock()
			fp.queryVal["collects_id"] = r.URL.Query().Get("collects_id")
			fp.mu.Unlock()
			_, _ = fmt.Fprint(w, `{"items":[{"aweme_id":"v1"}]}`)
		case "/unread":
			_, _ = fmt.Fprint(w, `{"unread_count":5,"conv_list":[{"conversation_id":"c1","unread_count":2}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fp.srv.Close)
	return fp
}

// writeM6AdaptDir creates a temp adapt dir whose douyin contracts point at
// the fake platform (contracts declare behavior; tests never hardcode
// endpoints in the command layer).
func writeM6AdaptDir(t *testing.T, baseURL string) string {
	t.Helper()
	dir := t.TempDir()
	cdir := filepath.Join(dir, "contracts")
	for _, sub := range []string{"contracts", "fixtures", "canaries"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	contracts := map[string]string{
		"douyin-search.json": `{"name":"douyin-search","platform":"douyin","category":"search","version":"1",
			"transport":{"base_url":%q,"path":"/search","method":"GET"},
			"binding":{"items":"$.items"}}`,
		"douyin-video-download.json": `{"name":"douyin-video-download","platform":"douyin","category":"video_download","version":"1",
			"transport":{"base_url":%q,"path":"/detail","method":"GET"},
			"binding":{"fields":{"play_url":"$.aweme_detail.video.play_addr.url_list[0]","cover":"$.aweme_detail.video.cover.url_list[0]","aweme_id":"$.aweme_detail.aweme_id"}}}`,
		"douyin-collects.json": `{"name":"douyin-collects","platform":"douyin","category":"collects","version":"1",
			"transport":{"base_url":%q,"path":"/collects","method":"GET"},
			"binding":{"items":"$.collects"},
			"paging":{"cursor_param":"cursor","count_param":"count","has_more_path":"$.has_more","next_cursor_path":"$.cursor"}}`,
		"douyin-collects-videos.json": `{"name":"douyin-collects-videos","platform":"douyin","category":"collects_videos","version":"1",
			"transport":{"base_url":%q,"path":"/collects-videos","method":"GET"},
			"binding":{"items":"$.items"}}`,
		"douyin-im-unread.json": `{"name":"douyin-im-unread","platform":"douyin","category":"im_unread","version":"1",
			"transport":{"base_url":%q,"path":"/unread","method":"GET"},
			"binding":{"fields":{"total_unread":"$.unread_count"}}}`,
	}
	for name, tmpl := range contracts {
		if err := os.WriteFile(filepath.Join(cdir, name), []byte(fmt.Sprintf(tmpl, baseURL)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// setupM6 wires the fake platform + temp adapt dir for the functional
// collect tests.
func setupM6(t *testing.T) *fakePlatform {
	t.Helper()
	fp := newFakePlatform(t)
	t.Setenv("MEDIAMON_ADAPT_DIR", writeM6AdaptDir(t, fp.srv.URL))
	return fp
}

func TestParseAwemeID(t *testing.T) {
	id, err := parseAwemeID("https://www.douyin.com/video/7350000000000000001?foo=bar")
	if err != nil || id != "7350000000000000001" {
		t.Fatalf("url parse = %q, %v", id, err)
	}
	id, err = parseAwemeID("7350000000000000001")
	if err != nil || id != "7350000000000000001" {
		t.Fatalf("bare id = %q, %v", id, err)
	}
	if _, err := parseAwemeID(""); err == nil {
		t.Fatal("empty input accepted")
	}
	if _, err := parseAwemeID("https://www.douyin.com/"); err == nil {
		t.Fatal("url without id accepted")
	}
}

func TestCollectVideoResolveAndDownload(t *testing.T) {
	setupM6(t)
	outDir := t.TempDir()
	out, err := captureStdout(t, func() error {
		return cmdCollect([]string{"video", "--platform", "douyin", "--aweme-id", "AID1", "--download", "--out-dir", outDir})
	})
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(out)
	var meta struct {
		AwemeID string `json:"aweme_id"`
		URL     string `json:"url"`
		Cover   string `json:"cover"`
		Bytes   int64  `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(line), &meta); err != nil {
		t.Fatalf("output %q is not JSON: %v", line, err)
	}
	if meta.AwemeID != "AID1" || !strings.HasSuffix(meta.URL, "/video.mp4") || meta.Cover == "" {
		t.Fatalf("meta = %+v", meta)
	}
	raw, err := os.ReadFile(filepath.Join(outDir, "AID1.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "fake-video-bytes" {
		t.Fatalf("downloaded bytes = %q", raw)
	}
	if meta.Bytes != int64(len("fake-video-bytes")) {
		t.Fatalf("bytes = %d", meta.Bytes)
	}
}

func TestCollectVideoURLFlag(t *testing.T) {
	setupM6(t)
	out, err := captureStdout(t, func() error {
		return cmdCollect([]string{"video", "--platform", "douyin", "--url", "https://www.douyin.com/video/AID1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"aweme_id":"AID1"`) {
		t.Fatalf("output = %q", out)
	}
}

func TestCollectCollectsPagination(t *testing.T) {
	setupM6(t)
	out, err := captureStdout(t, func() error {
		return cmdCollect([]string{"collects", "--platform", "douyin", "--limit", "10"})
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("collects rows = %q (want 2 pages merged)", out)
	}
}

func TestCollectCollectsVideos(t *testing.T) {
	fp := setupM6(t)
	out, err := captureStdout(t, func() error {
		return cmdCollect([]string{"collects-videos", "--platform", "douyin", "--folder-id", "F9"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "v1") {
		t.Fatalf("output = %q", out)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.queryVal["collects_id"] != "F9" {
		t.Fatalf("collects_id query = %q", fp.queryVal["collects_id"])
	}
	if err := cmdCollect([]string{"collects-videos", "--platform", "douyin"}); err == nil {
		t.Fatal("missing --folder-id accepted")
	}
}

func TestCollectIMUnread(t *testing.T) {
	setupM6(t)
	out, err := captureStdout(t, func() error {
		return cmdCollect([]string{"im-unread", "--platform", "douyin"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		TotalUnread   int64            `json:"total_unread"`
		Conversations []map[string]any `json:"conversations"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err != nil {
		t.Fatalf("output %q is not JSON: %v", out, err)
	}
	if res.TotalUnread != 5 || len(res.Conversations) != 1 {
		t.Fatalf("unread = %+v", res)
	}
}

func TestCollectAccountInjection(t *testing.T) {
	fp := setupM6(t)
	acctDir := filepath.Join(t.TempDir(), "accounts")
	t.Setenv("MEDIAMON_ACCOUNTS_DIR", acctDir)
	pool, err := accounts.Open(acctDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Save(accounts.Account{
		ID: "acc-1", Platform: "douyin",
		Cookies: map[string]string{"ttwid": "acct-token"},
		UA:      "acct-ua",
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return cmdCollect([]string{"search", "--platform", "douyin", "--keyword", "kw1", "--account", "acc-1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "it1") {
		t.Fatalf("output = %q", out)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.cookie["/search"] != "ttwid=acct-token" {
		t.Fatalf("Cookie = %q, want account cookie", fp.cookie["/search"])
	}
	if fp.ua["/search"] != "acct-ua" {
		t.Fatalf("User-Agent = %q, want account UA", fp.ua["/search"])
	}
	if fp.queryVal["keyword"] != "kw1" {
		t.Fatalf("keyword = %q", fp.queryVal["keyword"])
	}

	// Unknown account id fails closed before any request.
	if err := cmdCollect([]string{"search", "--platform", "douyin", "--keyword", "kw1", "--account", "ghost"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown account error = %v", err)
	}
}
