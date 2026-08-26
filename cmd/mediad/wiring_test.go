package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/datacenter"
	"github.com/Cloudbird-Software/Media-Monitor/internal/license"
)

// writeM6Contracts adds the M6 demo contracts (video-download, collects,
// collects-videos, im-unread) for the demo platform on top of writeDemoAdapt.
func writeM6Contracts(t *testing.T, adaptDir, base string) {
	t.Helper()
	contracts := map[string]string{
		"demo-video-download.json": `{
		  "name": "demo-video-download",
		  "platform": "demo",
		  "category": "video_download",
		  "version": "1",
		  "doc": "test-only contract",
		  "transport": {"base_url": "` + base + `", "path": "/video/{aweme_id}", "method": "GET", "placeholders": ["aweme_id"]},
		  "binding": {"fields": {"play_url": "$.data.play", "cover": "$.data.cover", "aweme_id": "$.data.id"}}
		}`,
		"demo-collects.json": `{
		  "name": "demo-collects",
		  "platform": "demo",
		  "category": "collects",
		  "version": "1",
		  "doc": "test-only contract",
		  "transport": {"base_url": "` + base + `", "path": "/collects", "method": "GET"},
		  "binding": {"items": "$.data.list"},
		  "paging": {"has_more_path": "$.has_more"}
		}`,
		"demo-collects-videos.json": `{
		  "name": "demo-collects-videos",
		  "platform": "demo",
		  "category": "collects_videos",
		  "version": "1",
		  "doc": "test-only contract",
		  "transport": {"base_url": "` + base + `", "path": "/collects/{collects_id}/videos", "method": "GET", "placeholders": ["collects_id"]},
		  "binding": {"items": "$.data.list"},
		  "paging": {"has_more_path": "$.has_more"}
		}`,
		"demo-im-unread.json": `{
		  "name": "demo-im-unread",
		  "platform": "demo",
		  "category": "im_unread",
		  "version": "1",
		  "doc": "test-only contract",
		  "transport": {"base_url": "` + base + `", "path": "/im/unread", "method": "GET"},
		  "binding": {"fields": {"total_unread": "$.total_unread"}}
		}`,
	}
	for name, content := range contracts {
		if err := os.WriteFile(filepath.Join(adaptDir, "contracts", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// m6API serves the demo M6 endpoints.
func m6API(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/video/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"play":["https://cdn.test/v.mp4"],"cover":["https://cdn.test/c.jpg"],"id":"aw1"}}`)
	})
	mux.HandleFunc("/collects", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"list":[{"id":"f1","desc":"folder"}]},"has_more":false}`)
	})
	mux.HandleFunc("/collects/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"list":[{"id":"v9","desc":"clip"}]},"has_more":false}`)
	})
	mux.HandleFunc("/im/unread", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"total_unread":7,"conv_list":[{"cid":"c1"}]}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[],"has_more":false}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCollectAccountIDHeaders(t *testing.T) {
	var gotCookie, gotUA atomic.Value
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie.Store(r.Header.Get("Cookie"))
		gotUA.Store(r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[],"has_more":false}`)
	}))
	defer api.Close()

	// Shared HTTP client UA pool via MEDIAMON_UA_POOL (file present).
	uaFile := filepath.Join(t.TempDir(), "ua-pool.json")
	if err := os.WriteFile(uaFile, []byte(`{"uas":["PoolUA/1.0"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEDIAMON_UA_POOL", uaFile)

	d, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), writeDemoAdapt(t, api.URL))
	if d.accounts == nil {
		t.Fatal("account pool not wired")
	}
	if err := d.accounts.Save(accounts.Account{
		ID: "acct1", Platform: "demo",
		Cookies: map[string]string{"sess": "abc"},
		UA:      "AcctUA/1.0",
	}); err != nil {
		t.Fatal(err)
	}

	// No account_id: platform defaults — no cookie, UA from the shared pool.
	resp, b := postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo", "keyword": "k"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	if gotCookie.Load() != "" {
		t.Fatalf("cookie without account = %q, want empty", gotCookie.Load())
	}
	if gotUA.Load() != "PoolUA/1.0" {
		t.Fatalf("UA without account = %q, want PoolUA/1.0 (shared pool)", gotUA.Load())
	}

	// account_id: the account's cookie + pinned UA ride the request.
	resp, b = postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo", "keyword": "k", "account_id": "acct1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	if gotCookie.Load() != "sess=abc" {
		t.Fatalf("cookie with account = %q, want sess=abc", gotCookie.Load())
	}
	if gotUA.Load() != "AcctUA/1.0" {
		t.Fatalf("UA with account = %q, want AcctUA/1.0 (account pinned)", gotUA.Load())
	}

	// Unknown account_id falls back to the platform defaults.
	resp, b = postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo", "keyword": "k", "account_id": "nope"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	if gotCookie.Load() != "" || gotUA.Load() != "PoolUA/1.0" {
		t.Fatalf("unknown account fell back wrong: cookie=%q UA=%q", gotCookie.Load(), gotUA.Load())
	}
}

func TestLicenseGateREST(t *testing.T) {
	pub, _, err := license.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEDIAMON_LICENSE_REQUIRED", "true")
	t.Setenv("MEDIAMON_LICENSE_PUBKEY", base64.StdEncoding.EncodeToString(pub))

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[],"has_more":false}`)
	}))
	defer api.Close()

	// No license file in <data>/license: the gate denies with no_license.
	_, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), writeDemoAdapt(t, api.URL))

	resp, b := postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo", "keyword": "k"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("collect status = %d, body = %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "license_denied") || !strings.Contains(string(b), "no_license") {
		t.Fatalf("collect denial body = %s", b)
	}
	resp, b = postJSON(t, ts, "/api/v1/send", map[string]any{"platform": "demo"})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "no_license") {
		t.Fatalf("send status = %d, body = %s", resp.StatusCode, b)
	}
	resp, b = postJSON(t, ts, "/api/v1/tasks", map[string]any{"kind": "search"})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "no_license") {
		t.Fatalf("tasks POST status = %d, body = %s", resp.StatusCode, b)
	}

	// Exempt surfaces stay open.
	for _, path := range []string{"/healthz", "/metrics", "/", "/api/v1/tasks", "/api/v1/accounts"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("exempt GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestLicenseGateDisabledByEnv(t *testing.T) {
	pub, _, err := license.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEDIAMON_LICENSE_REQUIRED", "false")
	t.Setenv("MEDIAMON_LICENSE_PUBKEY", base64.StdEncoding.EncodeToString(pub))

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[],"has_more":false}`)
	}))
	defer api.Close()
	_, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), writeDemoAdapt(t, api.URL))

	resp, b := postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo", "keyword": "k"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("collect with gate disabled status = %d, body = %s", resp.StatusCode, b)
	}
	resp, err = http.Post(ts.URL+"/api/v1/tasks", "application/json", strings.NewReader(`{"kind":"search","config":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("tasks POST with gate disabled status = %d", resp.StatusCode)
	}
}

func TestDatacenterIngestAfterCollect(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"1","desc":"first","create_time":1700000000,"author":{"user_id":"u1","sec_uid":"s1","nickname":"nick"}}],"has_more":false}`)
	}))
	defer api.Close()
	d, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), writeDemoAdapt(t, api.URL))
	if d.hub == nil {
		t.Fatal("datacenter hub not wired")
	}
	if !strings.Contains(d.webhookDesc, "disabled") {
		t.Fatalf("webhookDesc without URL = %q, want disabled note", d.webhookDesc)
	}

	resp, b := postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo", "keyword": "k"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("collect status = %d, body = %s", resp.StatusCode, b)
	}
	recs := d.hub.List(nil, false)
	if len(recs) != 1 {
		t.Fatalf("hub records = %d, want 1", len(recs))
	}
	if recs[0].Platform != "demo" || recs[0].UserKey != "s1" || recs[0].Nickname != "nick" {
		t.Fatalf("hub record = %+v", recs[0])
	}
	stats := d.datacenterStats()
	if stats.Ingested != 1 || stats.Added != 1 || stats.Stored != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestDatacenterWebhookPushLoop(t *testing.T) {
	var pushes atomic.Int64
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer hook.Close()

	t.Setenv("MEDIAMON_WEBHOOK_URL", hook.URL)
	t.Setenv("MEDIAMON_WEBHOOK_MIN_INTERVAL", "1ms")
	t.Setenv("MEDIAMON_WEBHOOK_MAX_INTERVAL", "20ms")

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[],"has_more":false}`)
	}))
	defer api.Close()
	d, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), writeDemoAdapt(t, api.URL))
	if d.hub == nil {
		t.Fatal("datacenter hub not wired")
	}
	if !strings.Contains(d.webhookDesc, hook.URL) {
		t.Fatalf("webhookDesc = %q, want configured url", d.webhookDesc)
	}
	_ = ts

	// One record to flush; the push loop must trigger PushIfDue on its tick.
	d.hubAdd(datacenter.Record{Platform: "demo", UserKey: "u1"})
	d.pushInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.startPushLoop(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for pushes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pushes.Load() == 0 {
		t.Fatal("PushIfDue was never triggered by the push loop")
	}
}

func TestDashboardDatacenterAndIMSections(t *testing.T) {
	api := m6API(t)
	adaptDir := writeDemoAdapt(t, api.URL)
	writeM6Contracts(t, adaptDir, api.URL)
	d, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), adaptDir)

	d.pollIMUnreadOnce(context.Background(), "demo", "acct-x")

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	page := string(b)
	for _, want := range []string{"datacenter", "webhook", "IM unread", "acct-x", "<td>7</td>"} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard misses %q", want)
		}
	}
}

func TestCollectM6Endpoints(t *testing.T) {
	api := m6API(t)
	adaptDir := writeDemoAdapt(t, api.URL)
	writeM6Contracts(t, adaptDir, api.URL)
	_, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), adaptDir)

	// video: watermark-free address + cover metadata.
	resp, b := postJSON(t, ts, "/api/v1/collect/video", map[string]any{"platform": "demo", "item_id": "aw1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("video status = %d, body = %s", resp.StatusCode, b)
	}
	var videoOut struct {
		Video struct {
			AwemeID string `json:"aweme_id"`
			URL     string `json:"url"`
			Cover   string `json:"cover"`
		} `json:"video"`
	}
	if err := json.Unmarshal(b, &videoOut); err != nil {
		t.Fatal(err)
	}
	if videoOut.Video.URL != "https://cdn.test/v.mp4" || videoOut.Video.Cover != "https://cdn.test/c.jpg" || videoOut.Video.AwemeID != "aw1" {
		t.Fatalf("video = %+v", videoOut.Video)
	}

	// collects: folder list.
	resp, b = postJSON(t, ts, "/api/v1/collect/collects", map[string]any{"platform": "demo"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"f1"`) {
		t.Fatalf("collects status = %d, body = %s", resp.StatusCode, b)
	}

	// collects-videos: videos of one folder.
	resp, b = postJSON(t, ts, "/api/v1/collect/collects-videos", map[string]any{"platform": "demo", "collects_id": "f1"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"v9"`) {
		t.Fatalf("collects-videos status = %d, body = %s", resp.StatusCode, b)
	}
	resp, b = postJSON(t, ts, "/api/v1/collect/collects-videos", map[string]any{"platform": "demo"})
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(b), "collects_id is required") {
		t.Fatalf("collects-videos missing id status = %d, body = %s", resp.StatusCode, b)
	}

	// im-unread.
	resp, b = postJSON(t, ts, "/api/v1/collect/im-unread", map[string]any{"platform": "demo"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("im-unread status = %d, body = %s", resp.StatusCode, b)
	}
	var imOut struct {
		IMUnread struct {
			TotalUnread int64 `json:"total_unread"`
		} `json:"im_unread"`
	}
	if err := json.Unmarshal(b, &imOut); err != nil {
		t.Fatal(err)
	}
	if imOut.IMUnread.TotalUnread != 7 {
		t.Fatalf("im_unread = %+v", imOut.IMUnread)
	}
}

func TestIMPollOnceAndTask(t *testing.T) {
	api := m6API(t)
	adaptDir := writeDemoAdapt(t, api.URL)
	writeM6Contracts(t, adaptDir, api.URL)
	d, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), adaptDir)

	// Direct call: status map + hub record update.
	d.pollIMUnreadOnce(context.Background(), "demo", "acct-x")
	snap := d.im.snapshot()
	if len(snap) != 1 {
		t.Fatalf("im snapshot = %+v", snap)
	}
	s := snap[0]
	if s.Platform != "demo" || s.AccountID != "acct-x" || s.TotalUnread != 7 || s.Conversations != 1 || s.Error != "" {
		t.Fatalf("im status = %+v", s)
	}
	if s.LastPoll == 0 {
		t.Fatal("LastPoll not set")
	}
	recs := d.hub.List(nil, false)
	if len(recs) != 1 || recs[0].UserKey != "acct-x" {
		t.Fatalf("hub records = %+v", recs)
	}

	// Via the tasks API: an im-unread-poll task starts its polling loop.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.ctx = ctx

	resp, b := postJSON(t, ts, "/api/v1/tasks", map[string]any{
		"kind":   "im-unread-poll",
		"config": map[string]any{"platform": "demo", "account_id": "acct-y", "interval_seconds": 3600},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("submit status = %d, body = %s", resp.StatusCode, b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		found := false
		for _, st := range d.im.snapshot() {
			if st.AccountID == "acct-y" && st.TotalUnread == 7 {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("poll for acct-y never landed: %+v", d.im.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
