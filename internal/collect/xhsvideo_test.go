package collect

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

// xhsFeedFixture wires a registry whose xhs-video-download contract points
// at srv, plus an xhs signer that records what it was asked to sign.
func xhsFeedFixture(t *testing.T, srvURL string, signer httpclient.Signer) *Engine {
	t.Helper()
	contract := fmt.Sprintf(`{
	  "name": "xhs-video-download", "platform": "xhs", "category": "video_download", "version": "1",
	  "transport": {
	    "base_url": %q, "path": "/api/sns/web/v1/feed", "method": "POST",
	    "headers": {"origin": "https://www.xiaohongshu.com"},
	    "body": {"source_note_id": "", "image_scenes": ["FD_PRV_WEBP", "FD_WM_WEBP"]},
	    "placeholders": ["source_note_id"]
	  },
	  "signature": {"headers": ["x-s", "x-s-common"], "required": ["x-s", "x-s-common"]},
	  "cookie": {"required": ["web_session", "a1"]},
	  "binding": {"fields": {
	    "play_url": "$.data.items[0].note.video.media.stream.h264[0].master_url",
	    "cover": "$.data.items[0].note.image_list[0].url_default",
	    "aweme_id": "$.data.items[0].id"
	  }}
	}`, srvURL)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "xhs-video-download.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, dir); err != nil {
		t.Fatal(err)
	}
	eng := New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 5 * time.Second, UserAgents: []string{"xhs-test"}}),
		Obs:      obs.NewCounterMap(),
		Cookies:  map[string]string{"xhs": "web_session=sess; a1=abc"},
	})
	if signer != nil {
		eng.signers = map[string]httpclient.Signer{"xhs": signer}
	}
	return eng
}

// TestXhsHeaderSignatureRidesHeaders (IFACE-7): the signer output declared
// in signature.headers reaches the server as HTTP request headers (not
// query params), the undeclared extra key stays in the query, and the POST
// body carries the placeholder value.
func TestXhsHeaderSignatureRidesHeaders(t *testing.T) {
	var mu sync.Mutex
	var gotXS, gotXSCommon, gotQueryXS, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotXS = r.Header.Get("x-s")
		gotXSCommon = r.Header.Get("x-s-common")
		gotQueryXS = r.URL.Query().Get("x-s")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"items":[{"id":"xhs-note-video-0001","note":{"type":"video","video":{"media":{"stream":{"h264":[{"master_url":"https://sns-video-hw.xhscdn.com/f.mp4"}]}}}}}]},"code":0}`)
	}))
	defer srv.Close()

	eng := xhsFeedFixture(t, srv.URL, httpclient.StaticSigner{Fn: func(_ context.Context, _, _ string, params map[string]string) (map[string]string, error) {
		return map[string]string{"x-s": "signed-xs", "x-s-common": "signed-xsc", "x-legacy": "goes-to-query"}, nil
	}})
	meta, err := eng.ResolveVideo(context.Background(), "xhs", "note-0001")
	if err != nil {
		t.Fatalf("ResolveVideo: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if meta.URL != "https://sns-video-hw.xhscdn.com/f.mp4" || meta.AwemeID != "xhs-note-video-0001" {
		t.Fatalf("meta wrong: %+v", meta)
	}
	if gotXS != "signed-xs" || gotXSCommon != "signed-xsc" {
		t.Fatalf("signed headers missing at server: x-s=%q x-s-common=%q", gotXS, gotXSCommon)
	}
	if gotQueryXS != "" {
		t.Fatal("x-s must not ride the query when declared in signature.headers")
	}
	if !strings.Contains(gotBody, `"source_note_id":"note-0001"`) {
		t.Fatalf("placeholder must reach the POST body: %s", gotBody)
	}
}

// TestXhsHeaderSignatureFailClosed: a signer that omits a required header
// value fails closed with an explicit error — no unsigned request is sent.
func TestXhsHeaderSignatureFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("no request may be sent when a required signature header is missing")
	}))
	defer srv.Close()
	eng := xhsFeedFixture(t, srv.URL, httpclient.StaticSigner{Fn: func(context.Context, string, string, map[string]string) (map[string]string, error) {
		return map[string]string{"x-s": "only-xs"}, nil
	}})
	_, err := eng.ResolveVideo(context.Background(), "xhs", "note-0001")
	if err == nil || !strings.Contains(err.Error(), `signature required header "x-s-common"`) {
		t.Fatalf("want explicit header fail-closed, got %v", err)
	}
}

// TestXhsImageNoteNoPlayURL: an image-text note binds no play_url — resolve
// fails closed with an explicit error instead of guessing (fixture 2 shape).
func TestXhsImageNoteNoPlayURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"items":[{"id":"xhs-note-imagetext-0002","note":{"type":"normal","image_list":[{"url_default":"https://sns-img-qc.xhscdn.com/inner.webp"}]}}]},"code":0}`)
	}))
	defer srv.Close()
	eng := xhsFeedFixture(t, srv.URL, httpclient.StaticSigner{Fn: func(context.Context, string, string, map[string]string) (map[string]string, error) {
		return map[string]string{"x-s": "x", "x-s-common": "y"}, nil
	}})
	_, err := eng.ResolveVideo(context.Background(), "xhs", "note-0002")
	if err == nil || !strings.Contains(err.Error(), "no play URL") {
		t.Fatalf("want explicit no-play-URL fail-closed, got %v", err)
	}
}

// TestXhsNoSignerFailClosed: no signer wired for the platform → the required
// header can never be satisfied; the request fails closed (pre-existing
// INV-1 semantics extended to headers).
func TestXhsNoSignerFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("no request may be sent without a signer")
	}))
	defer srv.Close()
	eng := xhsFeedFixture(t, srv.URL, nil)
	_, err := eng.ResolveVideo(context.Background(), "xhs", "note-0001")
	if err == nil || !strings.Contains(err.Error(), "signature required") {
		t.Fatalf("want fail-closed without signer, got %v", err)
	}
}
