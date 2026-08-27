package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

// dlEnv wires a video-download contract (detail endpoint) plus a CDN server
// serving the bytes.
type dlEnv struct {
	api *httptest.Server
	cdn *httptest.Server
	eng *Engine
}

func newDlEnv(t *testing.T, cdnStatus int) *dlEnv {
	t.Helper()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cdnStatus != 0 {
			w.WriteHeader(cdnStatus)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("fake-video-bytes-0123456789"))
	}))
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"aweme_detail":{"aweme_id":"aw-1","video":{"play_addr":{"url_list":[%q]},"cover":{"url_list":["https://cdn.invalid/c.jpg"]}}}}`, cdn.URL+"/v/aw-1.mp4")
	}))
	dir := t.TempDir()
	contract := fmt.Sprintf(`{
	  "name": "douyin-video-download", "platform": "douyin", "category": "video_download", "version": "1",
	  "transport": {"base_url": %q, "path": "/aweme/v1/web/aweme/detail/", "method": "GET", "placeholders": ["aweme_id"]},
	  "binding": {"items": "", "fields": {"play_url": "$.aweme_detail.video.play_addr.url_list", "cover": "$.aweme_detail.video.cover.url_list", "aweme_id": "$.aweme_detail.aweme_id"}}
	}`, api.URL)
	if err := os.WriteFile(filepath.Join(dir, "douyin-video-download.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, dir); err != nil {
		t.Fatal(err)
	}
	return &dlEnv{
		api: api, cdn: cdn,
		eng: New(Context{
			Registry: reg,
			HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}),
			Obs:      obs.NewCounterMap(),
			Names:    map[string]map[string]string{"douyin": {"video_download": "douyin-video-download"}},
		}),
	}
}

// TestDownloadVideoToResult: {path, bytes, sha256} all check out, the file
// lands in the IFACE-3 layout <out>/<platform>/<item>.mp4, and the hash is
// recomputable from the file content (W3-C3 AC-1/AC-2).
func TestDownloadVideoToResult(t *testing.T) {
	env := newDlEnv(t, 0)
	defer env.api.Close()
	defer env.cdn.Close()
	out := t.TempDir()

	res, err := env.eng.DownloadVideoTo(context.Background(), "douyin", "aw-1", out)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(out, "douyin", "aw-1.mp4")
	if res.Path != wantPath {
		t.Fatalf("path = %q, want %q", res.Path, wantPath)
	}
	if res.Bytes != int64(len("fake-video-bytes-0123456789")) {
		t.Fatalf("bytes = %d", res.Bytes)
	}
	raw, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
	sum := sha256.Sum256(raw)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 mismatch: %s vs file %s", res.SHA256, hex.EncodeToString(sum[:]))
	}
}

// TestDownloadVideoToFailureNoResidue: a non-2xx CDN answer is an explicit
// error and leaves neither the final file nor the .tmp sibling (AC-3).
func TestDownloadVideoToFailureNoResidue(t *testing.T) {
	env := newDlEnv(t, http.StatusForbidden)
	defer env.api.Close()
	defer env.cdn.Close()
	out := t.TempDir()

	_, err := env.eng.DownloadVideoTo(context.Background(), "douyin", "aw-1", out)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want explicit 403 download error", err)
	}
	dir := filepath.Join(out, "douyin")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Fatalf("residue after failure: %s", e.Name())
	}
}

// TestDownloadVideoToStreams: the engine download path rides DoStream
// (structural: engine.Download uses hc.DoStream; a served body larger than
// any plausible buffer still lands byte-exact — the streaming sanity
// proxy).
func TestDownloadVideoToStreams(t *testing.T) {
	big := strings.Repeat("x", 1<<20) // 1 MiB body
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer cdn.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"aweme_detail":{"aweme_id":"aw-2","video":{"play_addr":{"url_list":[%q]}}}}`, cdn.URL+"/big.mp4")
	}))
	defer api.Close()
	dir := t.TempDir()
	contract := fmt.Sprintf(`{"name":"douyin-video-download","platform":"douyin","category":"video_download","version":"1","transport":{"base_url":%q,"path":"/d/","method":"GET","placeholders":["aweme_id"]},"binding":{"fields":{"play_url":"$.aweme_detail.video.play_addr.url_list","aweme_id":"$.aweme_detail.aweme_id"}}}`, api.URL)
	if err := os.WriteFile(filepath.Join(dir, "douyin-video-download.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, dir); err != nil {
		t.Fatal(err)
	}
	eng := New(Context{Registry: reg, HTTP: httpclient.New(httpclient.Config{}), Obs: obs.NewCounterMap(),
		Names: map[string]map[string]string{"douyin": {"video_download": "douyin-video-download"}}})
	res, err := eng.DownloadVideoTo(context.Background(), "douyin", "aw-2", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes != int64(len(big)) {
		t.Fatalf("bytes = %d, want %d (streamed whole)", res.Bytes, len(big))
	}
}
