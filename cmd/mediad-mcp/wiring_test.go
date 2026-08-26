package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// writeM6AdaptDir lays out the base adapt tree plus douyin M6 contracts
// (video-download, collects, collects-videos, im-unread) pointing at the
// given test server. The douyin Names map declares none of these categories,
// so the engine resolves them by the "<platform>-<suffix>" convention.
func writeM6AdaptDir(t *testing.T, base string) string {
	t.Helper()
	dir := writeAdaptDir(t)
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("contracts/douyin-video-download.json", `{
	  "name": "douyin-video-download",
	  "platform": "douyin",
	  "category": "video_download",
	  "version": "1",
	  "doc": "test-only contract",
	  "transport": {"base_url": "`+base+`", "path": "/video/{aweme_id}", "method": "GET", "placeholders": ["aweme_id"]},
	  "binding": {"fields": {"play_url": "$.data.play", "cover": "$.data.cover", "aweme_id": "$.data.id"}}
	}`)
	mustWrite("contracts/douyin-collects.json", `{
	  "name": "douyin-collects",
	  "platform": "douyin",
	  "category": "collects",
	  "version": "1",
	  "doc": "test-only contract",
	  "transport": {"base_url": "`+base+`", "path": "/collects", "method": "GET"},
	  "binding": {"items": "$.data.list"},
	  "paging": {"has_more_path": "$.has_more"}
	}`)
	mustWrite("contracts/douyin-collects-videos.json", `{
	  "name": "douyin-collects-videos",
	  "platform": "douyin",
	  "category": "collects_videos",
	  "version": "1",
	  "doc": "test-only contract",
	  "transport": {"base_url": "`+base+`", "path": "/collects/{collects_id}/videos", "method": "GET", "placeholders": ["collects_id"]},
	  "binding": {"items": "$.data.list"},
	  "paging": {"has_more_path": "$.has_more"}
	}`)
	mustWrite("contracts/douyin-im-unread.json", `{
	  "name": "douyin-im-unread",
	  "platform": "douyin",
	  "category": "im_unread",
	  "version": "1",
	  "doc": "test-only contract",
	  "transport": {"base_url": "`+base+`", "path": "/im/unread", "method": "GET"},
	  "binding": {"fields": {"total_unread": "$.total_unread"}}
	}`)
	return dir
}

// m6API serves the douyin M6 endpoints and records the last request headers.
func m6API(t *testing.T, gotCookie, gotUA *atomic.Value) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		if gotCookie != nil {
			gotCookie.Store(r.Header.Get("Cookie"))
		}
		if gotUA != nil {
			gotUA.Store(r.Header.Get("User-Agent"))
		}
	}
	mux.HandleFunc("/video/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_, _ = io.WriteString(w, `{"data":{"play":["https://cdn.test/v.mp4"],"cover":["https://cdn.test/c.jpg"],"id":"aw1"}}`)
	})
	mux.HandleFunc("/collects", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_, _ = io.WriteString(w, `{"data":{"list":[{"id":"f1","desc":"folder"}]},"has_more":false}`)
	})
	mux.HandleFunc("/collects/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_, _ = io.WriteString(w, `{"data":{"list":[{"id":"v9","desc":"clip"}]},"has_more":false}`)
	})
	mux.HandleFunc("/im/unread", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_, _ = io.WriteString(w, `{"total_unread":7,"conv_list":[{"cid":"c1"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestM6Tools(t *testing.T) {
	var gotCookie, gotUA atomic.Value
	api := m6API(t, &gotCookie, &gotUA)
	t.Setenv("MEDIAMON_ADAPT_DIR", writeM6AdaptDir(t, api.URL))
	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("MEDIAMON_DATA_DIR", dataDir)
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	t.Setenv("MEDIAMON_LICENSE_REQUIRED", "false")

	// Seed one pool account (cookie + pinned UA) before the server opens it.
	pool, err := accounts.Open(filepath.Join(dataDir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Save(accounts.Account{
		ID: "acct1", Platform: "douyin",
		Cookies: map[string]string{"sess": "abc"},
		UA:      "AcctUA/1.0",
	}); err != nil {
		t.Fatal(err)
	}
	_ = pool.Close()

	c := startServer(t)

	// resolve_video: watermark-free address + cover metadata.
	out := c.callTool(t, "resolve_video", map[string]any{"platform": "douyin", "item_id": "aw1"})
	video := out["video"].(map[string]any)
	if video["url"] != "https://cdn.test/v.mp4" || video["cover"] != "https://cdn.test/c.jpg" || video["aweme_id"] != "aw1" {
		t.Fatalf("resolve_video = %v", video)
	}
	if gotCookie.Load() != "" {
		t.Fatalf("cookie without account = %q, want empty", gotCookie.Load())
	}

	// account_id routes the request through the account's cookie + UA.
	out = c.callTool(t, "resolve_video", map[string]any{"platform": "douyin", "item_id": "aw1", "account_id": "acct1"})
	if gotCookie.Load() != "sess=abc" {
		t.Fatalf("cookie with account = %q, want sess=abc", gotCookie.Load())
	}
	if gotUA.Load() != "AcctUA/1.0" {
		t.Fatalf("UA with account = %q, want AcctUA/1.0", gotUA.Load())
	}

	// get_collects: folder list, then the videos of one folder.
	out = c.callTool(t, "get_collects", map[string]any{"platform": "douyin"})
	collects := out["collects"].([]any)
	if len(collects) != 1 || collects[0].(map[string]any)["id"] != "f1" {
		t.Fatalf("get_collects = %v", out)
	}
	out = c.callTool(t, "get_collects", map[string]any{"platform": "douyin", "collects_id": "f1"})
	items := out["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "v9" {
		t.Fatalf("get_collects(collects_id) = %v", out)
	}

	// get_im_unread.
	out = c.callTool(t, "get_im_unread", map[string]any{"platform": "douyin"})
	im := out["im_unread"].(map[string]any)
	if im["total_unread"] != float64(7) {
		t.Fatalf("get_im_unread = %v", im)
	}

	// Validation still applies.
	msg := c.callToolErr(t, "resolve_video", map[string]any{"platform": "douyin"})
	if !strings.Contains(msg, "item_id is required") {
		t.Fatalf("resolve_video error = %q", msg)
	}
}

// TestLiveDecoderForPlatforms pins the monitor_live wiring: each platform
// resolves its own <platform>-meta contract and wire decoder.
func TestLiveDecoderForPlatforms(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("kuaishou-meta.json", `{
	  "name": "kuaishou-meta", "platform": "kuaishou", "category": "live_meta", "version": "1",
	  "transport": {"base_url": "https://live.kuaishou.com", "path": "/enter", "method": "GET"},
	  "binding": {"fields": {"room_id": "$.room_id"}},
	  "protocol_methods": {"SCWebFeedPush": "chat", "SCWebLikeMessage": "like"}
	}`)
	mustWrite("xhs-meta.json", `{
	  "name": "xhs-meta", "platform": "xhs", "category": "live_meta", "version": "1",
	  "transport": {"base_url": "https://www.xiaohongshu.com", "path": "/enter", "method": "GET"},
	  "binding": {"fields": {"room_id": "$.room_id"}},
	  "protocol_methods": {"CommentMessage": "chat", "LikeMessage": "like"}
	}`)
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, dir); err != nil {
		t.Fatal(err)
	}

	douyinDec := liveDecoderFor(reg, "douyin")
	ksDec := liveDecoderFor(reg, "kuaishou")
	xhsDec := liveDecoderFor(reg, "xhs")
	if douyinDec != nil {
		t.Fatalf("douyin decoder = %T, want nil (built-in protobuf path)", douyinDec)
	}
	if ksDec == nil || xhsDec == nil {
		t.Fatalf("kuaishou decoder = %v, xhs decoder = %v, want both non-nil", ksDec, xhsDec)
	}
	ksType := fmt.Sprintf("%T", ksDec)
	xhsType := fmt.Sprintf("%T", xhsDec)
	if ksType == xhsType {
		t.Fatalf("kuaishou and xhs decoders must differ, both %s", ksType)
	}
	if !strings.Contains(ksType, "kuaishou") || !strings.Contains(xhsType, "xhs") {
		t.Fatalf("decoder types = %s / %s", ksType, xhsType)
	}

	// A platform without a declared meta contract falls back to nil (douyin
	// protobuf path) instead of failing.
	if d := liveDecoderFor(reg, "nope"); d != nil {
		t.Fatalf("unknown platform decoder = %T, want nil", d)
	}
}

// TestCollectToolsCallableWithoutLicenseConfig: every previously gated
// collect/action tool answers its own validation without any license
// configuration (W1-C1 AC-4). Fail-before: with the gate in place these
// calls were refused with a structured license_denied error.
func TestCollectToolsCallableWithoutLicenseConfig(t *testing.T) {
	t.Setenv("MEDIAMON_ADAPT_DIR", writeAdaptDir(t))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c := startServer(t)

	// Each tool must reach its own argument validation (or signature
	// fail-closed path), never a license refusal.
	for _, tool := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"search_items", map[string]any{"platform": "douyin"}, "keyword is required"},
		{"get_comments", map[string]any{"platform": "douyin"}, "item_id is required"},
		{"get_replies", map[string]any{"platform": "douyin", "item_id": "i", "cid": "c"}, "a_bogus"},
		{"get_user", map[string]any{"platform": "douyin"}, "sec_uid is required"},
		{"group_members", map[string]any{"platform": "douyin"}, "group_id is required"},
		{"resolve_video", map[string]any{"platform": "douyin"}, "item_id is required"},
		{"get_collects", map[string]any{"platform": "douyin"}, "not declared"},
		{"get_im_unread", map[string]any{"platform": "douyin"}, "not declared"},
	} {
		msg := c.callToolErr(t, tool.name, tool.args)
		if strings.Contains(msg, "license_denied") {
			t.Fatalf("tool %s refused with license error: %q", tool.name, msg)
		}
		if tool.want != "" && !strings.Contains(msg, tool.want) {
			t.Fatalf("tool %s error = %q, want %q", tool.name, msg, tool.want)
		}
	}

	// send_message and adb_list answer without a license refusal too, but
	// through their structured (non-error) result surfaces.
	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"send_message", map[string]any{"platform": "douyin", "targets": "t", "first_message": "hi"}},
		{"adb_list", map[string]any{}},
	} {
		if out := c.callTool(t, tool.name, tool.args); out == nil {
			t.Fatalf("tool %s failed", tool.name)
		}
	}

	// Meta surfaces keep working (regression).
	for _, tool := range []string{"version", "contracts_list", "accounts_list", "adapt_canary_offline"} {
		if out := c.callTool(t, tool, map[string]any{}); out == nil {
			t.Fatalf("meta tool %s failed", tool)
		}
	}
}

// TestLicenseEnvNoOp: setting the retired MEDIAMON_LICENSE_* variables
// changes nothing (W1-C1 AC-3) — the behavior is identical to leaving them
// unset.
func TestLicenseEnvNoOp(t *testing.T) {
	t.Setenv("MEDIAMON_ADAPT_DIR", writeAdaptDir(t))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	t.Setenv("MEDIAMON_LICENSE_DIR", t.TempDir())
	t.Setenv("MEDIAMON_LICENSE_PUBKEY", "not-a-valid-key")
	t.Setenv("MEDIAMON_LICENSE_REQUIRED", "true")
	c := startServer(t)

	msg := c.callToolErr(t, "search_items", map[string]any{"platform": "douyin"})
	if !strings.Contains(msg, "keyword is required") {
		t.Fatalf("search_items error = %q, want validation error (env vars are no-ops)", msg)
	}
}
