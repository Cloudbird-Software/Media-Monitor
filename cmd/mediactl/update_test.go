package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveManifest serves a manifest pointing at the server's own /bin binary.
func serveManifest(t *testing.T, version string, bin []byte, sha string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"version":%q,"url":%q,"sha256":%q,"release_notes":"test release"}`,
				version, srv.URL+"/bin", sha)
		case "/bin":
			_, _ = w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestUpdateCheckReportsUpdate(t *testing.T) {
	bin := []byte("new-binary-bytes")
	srv := serveManifest(t, "9.9.9", bin, sha256Hex(bin))
	out, err := captureStdout(t, func() error {
		return updateCheck([]string{"--manifest-url", srv.URL + "/manifest.json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "current version: "+version) ||
		!strings.Contains(out, "update available: 9.9.9") ||
		!strings.Contains(out, "release notes: test release") {
		t.Fatalf("output = %q", out)
	}
}

func TestUpdateCheckDownloadVerifiesSHA256(t *testing.T) {
	bin := []byte("new-binary-bytes")
	srv := serveManifest(t, "9.9.9", bin, sha256Hex(bin))
	dest := filepath.Join(t.TempDir(), "updates")
	out, err := captureStdout(t, func() error {
		return updateCheck([]string{"--manifest-url", srv.URL + "/manifest.json", "--download", "--dest", dest})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "downloaded (sha256 verified):") {
		t.Fatalf("output = %q", out)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "9.9.9-") {
		t.Fatalf("dest entries = %v", entries)
	}
	raw, err := os.ReadFile(filepath.Join(dest, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(bin) {
		t.Fatalf("downloaded bytes = %q", raw)
	}
}

func TestUpdateCheckChecksumMismatchDiscards(t *testing.T) {
	bin := []byte("tampered-bytes")
	srv := serveManifest(t, "9.9.9", bin, sha256Hex([]byte("other")))
	dest := filepath.Join(t.TempDir(), "updates")
	err := updateCheck([]string{"--manifest-url", srv.URL + "/manifest.json", "--download", "--dest", dest})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 0 {
		t.Fatalf("mismatched download was kept: %v", entries)
	}
}

func TestUpdateCheckUpToDate(t *testing.T) {
	old := version
	version = "9.9.9"
	defer func() { version = old }()
	srv := serveManifest(t, "9.9.9", []byte("x"), "")
	out, err := captureStdout(t, func() error {
		return updateCheck([]string{"--manifest-url", srv.URL + "/manifest.json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "current version: 9.9.9") || !strings.Contains(out, "already up to date") {
		t.Fatalf("output = %q", out)
	}
}

func TestUpdateCheckRequiresManifestURL(t *testing.T) {
	t.Setenv("MEDIAMON_UPDATE_MANIFEST_URL", "")
	if err := updateCheck([]string{}); err == nil || !strings.Contains(err.Error(), "manifest-url") {
		t.Fatalf("error = %v", err)
	}
	if err := cmdUpdate([]string{"nope"}); err == nil {
		t.Fatal("unknown update subcommand accepted")
	}
}
