package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// dlAdaptDir lays out a douyin video-download contract against api.
func dlAdaptDir(t *testing.T, apiURL string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "contracts"), 0o755); err != nil {
		t.Fatal(err)
	}
	contract := fmt.Sprintf(`{
	  "name": "douyin-video-download", "platform": "douyin", "category": "video_download", "version": "1",
	  "transport": {"base_url": %q, "path": "/detail/", "method": "GET", "placeholders": ["aweme_id"]},
	  "binding": {"fields": {"play_url": "$.aweme_detail.video.play_addr.url_list", "aweme_id": "$.aweme_detail.aweme_id"}}
	}`, apiURL)
	if err := os.WriteFile(filepath.Join(dir, "contracts", "douyin-video-download.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDownloadVideoTool: the MCP tool returns the three-field result and
// the artifact lands at the IFACE-3 default layout under the data dir.
func TestDownloadVideoTool(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("mp4-bytes-here"))
	}))
	defer cdn.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"aweme_detail":{"aweme_id":"vid-7","video":{"play_addr":{"url_list":[%q]}}}}`, cdn.URL+"/v.mp4")
	}))
	defer api.Close()

	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("MEDIAMON_ADAPT_DIR", dlAdaptDir(t, api.URL))
	t.Setenv("MEDIAMON_DATA_DIR", dataDir)
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c := startServer(t)

	out := c.callTool(t, "download_video", map[string]any{"platform": "douyin", "item_id": "vid-7"})
	if out == nil {
		t.Fatal("download_video failed")
	}
	path, _ := out["path"].(string)
	want := filepath.Join(dataDir, "artifacts", "douyin", "vid-7.mp4")
	if path != want {
		t.Fatalf("path = %q, want default artifacts layout %q", path, want)
	}
	if out["bytes"] != float64(len("mp4-bytes-here")) {
		t.Fatalf("bytes = %v", out["bytes"])
	}
	raw, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
	sum := sha256.Sum256(raw)
	if out["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 = %v", out["sha256"])
	}
}
