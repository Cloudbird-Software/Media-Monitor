package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

// withTestAllowlist swaps the production image-CDN allowlist for a loopback
// one and restores it when the test ends.
func withTestAllowlist(t *testing.T, platform string) {
	t.Helper()
	old := cdnAllowlistOverride
	cdnAllowlistOverride = map[string][]string{platform: {"127.0.0.1"}}
	t.Cleanup(func() { cdnAllowlistOverride = old })
}

func newMediaEngine(t *testing.T) *Engine {
	t.Helper()
	return New(Context{
		HTTP: httpclient.New(httpclient.Config{Timeout: 5 * time.Second, UserAgents: []string{"media-test"}}),
		Obs:  obs.NewCounterMap(),
	})
}

// TestDownloadNoteImagesManifest: two allowlisted URLs stream to
// NNN.<ext> files with a manifest that carries per-file sha256 and the
// completion marker semantics (manifest written last).
func TestDownloadNoteImagesManifest(t *testing.T) {
	withTestAllowlist(t, "xhs")
	img1 := []byte("image-bytes-one")
	img2 := []byte("image-bytes-two-longer")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.webp":
			_, _ = w.Write(img1)
		case "/b.jpg":
			_, _ = w.Write(img2)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	eng := newMediaEngine(t)
	out := t.TempDir()
	m, err := eng.DownloadNoteImages(context.Background(), "xhs", "note-1",
		[]string{srv.URL + "/a.webp", srv.URL + "/b.jpg"}, out)
	if err != nil {
		t.Fatalf("DownloadNoteImages: %v", err)
	}
	if len(m.Files) != 2 || m.TotalBytes != int64(len(img1)+len(img2)) {
		t.Fatalf("manifest files/bytes wrong: %+v", m)
	}
	if filepath.Base(m.Files[0].Path) != "01.webp" || filepath.Base(m.Files[1].Path) != "02.jpg" {
		t.Fatalf("file naming wrong: %s, %s", m.Files[0].Path, m.Files[1].Path)
	}
	for i, want := range [][]byte{img1, img2} {
		got, rerr := os.ReadFile(m.Files[i].Path)
		if rerr != nil {
			t.Fatalf("read %s: %v", m.Files[i].Path, rerr)
		}
		sum := sha256.Sum256(want)
		if m.Files[i].SHA256 != hex.EncodeToString(sum[:]) || string(got) != string(want) {
			t.Fatalf("file %d content/sha mismatch", i+1)
		}
	}
	raw, rerr := os.ReadFile(m.ManifestPath)
	if rerr != nil {
		t.Fatalf("manifest missing: %v", rerr)
	}
	var back MediaManifest
	if jerr := json.Unmarshal(raw, &back); jerr != nil || len(back.Files) != 2 {
		t.Fatalf("manifest unreadable: %v", jerr)
	}
	// no tmp residue
	if ents, _ := os.ReadDir(filepath.Dir(m.Files[0].Path)); fileHasTmp(ents) {
		t.Fatal("tmp residue left behind")
	}
}

func fileHasTmp(ents []os.DirEntry) bool {
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			return true
		}
	}
	return false
}

// TestDownloadNoteImagesAllowlistClosed: a URL off the platform CDN
// allowlist fails closed before any byte is fetched (INV-1).
func TestDownloadNoteImagesAllowlistClosed(t *testing.T) {
	withTestAllowlist(t, "xhs")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should never be fetched"))
	}))
	defer srv.Close()
	eng := newMediaEngine(t)
	out := t.TempDir()
	// srv.URL host is 127.0.0.1 but path host is evil.example — the
	// allowlist decision runs on the URL's own host.
	_, err := eng.DownloadNoteImages(context.Background(), "xhs", "note-1",
		[]string{"https://evil.example/a.webp", srv.URL + "/a.webp"}, out)
	if err == nil || !strings.Contains(err.Error(), "cdn_host_not_allowed") {
		t.Fatalf("want cdn_host_not_allowed fail-closed, got %v", err)
	}
}

// TestDownloadNoteImagesManifestLast: a failure partway leaves no manifest
// behind (manifest presence = run completion marker).
func TestDownloadNoteImagesManifestLast(t *testing.T) {
	withTestAllowlist(t, "xhs")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok.webp" {
			_, _ = w.Write([]byte("fine"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	eng := newMediaEngine(t)
	out := t.TempDir()
	if _, err := eng.DownloadNoteImages(context.Background(), "xhs", "note-1",
		[]string{srv.URL + "/ok.webp", srv.URL + "/gone.webp"}, out); err == nil {
		t.Fatal("second image failure must error")
	}
	if _, serr := os.Stat(filepath.Join(out, "xhs", "note-1", "manifest.json")); !os.IsNotExist(serr) {
		t.Fatal("manifest must not exist after a failed run")
	}
}

// TestDownloadNoteImagesGuards: empty urls / bad item id / unknown platform
// allowlist all fail closed with explicit errors.
func TestDownloadNoteImagesGuards(t *testing.T) {
	withTestAllowlist(t, "xhs")
	eng := newMediaEngine(t)
	out := t.TempDir()
	if _, err := eng.DownloadNoteImages(context.Background(), "xhs", "note-1", nil, out); err == nil {
		t.Fatal("empty urls must fail closed")
	}
	if _, err := eng.DownloadNoteImages(context.Background(), "xhs", "../escape", []string{"https://a.xhscdn.com/x.webp"}, out); err == nil {
		t.Fatal("path-escape item id must fail closed")
	}
	if _, err := eng.DownloadNoteImages(context.Background(), "kuaishou", "note-1", []string{"https://a.xhscdn.com/x.webp"}, out); err == nil {
		t.Fatal("cross-platform host must fail the allowlist check")
	}
}
