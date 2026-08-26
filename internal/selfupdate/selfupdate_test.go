package selfupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionGreater(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.2.3", "1.2.0", true},
		{"1.2.0", "1.2.3", false},
		{"1.2.3", "1.2.3", false},
		{"2.0.0", "1.9.9", true},
		{"1.10.0", "1.9.0", true},
		{"", "1.0.0", false},
		{"1.0.0", "", true},
	}
	for _, c := range cases {
		if got := versionGreater(c.a, c.b); got != c.want {
			t.Fatalf("versionGreater(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCheckUpdateAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.2.4","url":"` + r.Host + `/bin","sha256":"abc"}`))
	}))
	defer srv.Close()
	c := NewChecker(srv.URL+"/manifest", "1.2.3", nil)
	m, err := c.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if m == nil || m.Version != "1.2.4" {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestCheckUpToDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.2.3","url":"x","sha256":"y"}`))
	}))
	defer srv.Close()
	c := NewChecker(srv.URL, "1.2.3", nil)
	m, err := c.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if m != nil {
		t.Fatalf("expected up to date, got %+v", m)
	}
}

func TestDownloadVerifyAndMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake-binary-bytes"))
	}))
	defer srv.Close()
	c := NewChecker(srv.URL, "1.0.0", nil)

	// Mismatch: checksum differs → error, no file written.
	m := &Manifest{
		Version: "1.1.0",
		URL:     srv.URL + "/x",
		SHA256:  "deadbeef",
	}
	destDir := t.TempDir()
	if _, err := c.Download(m, destDir); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	entries, _ := os.ReadDir(destDir)
	if len(entries) != 0 {
		t.Fatalf("mismatch must not write any file, got %v", entries)
	}
}

// TestDownloadHappyPath: correct SHA256 → the download lands in destDir with
// byte-identical content and a version+hash-derived file name.
func TestDownloadHappyPath(t *testing.T) {
	payload := []byte("fake-binary-bytes-v2\x00\x01\x02")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	c := NewChecker(srv.URL, "1.0.0", nil)

	m := &Manifest{
		Version: "1.2.0",
		URL:     srv.URL + "/bin",
		SHA256:  hex.EncodeToString(sum[:]),
	}
	destDir := filepath.Join(t.TempDir(), "updates") // must be created
	dest, err := c.Download(m, destDir)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	wantName := "1.2.0-" + hex.EncodeToString(sum[:])[:8] + ".bin"
	if filepath.Base(dest) != wantName {
		t.Fatalf("dest name = %q, want %q", filepath.Base(dest), wantName)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch: %q vs %q", got, payload)
	}
	// Uppercase hex in the manifest must also verify (EqualFold).
	m.SHA256 = strings.ToUpper(hex.EncodeToString(sum[:]))
	if _, err := c.Download(m, t.TempDir()); err != nil {
		t.Fatalf("uppercase sha256: %v", err)
	}
}

func TestCheckNoManifestURL(t *testing.T) {
	c := NewChecker("", "1.0.0", nil)
	if _, err := c.Check(); err == nil {
		t.Fatal("expected error for empty manifest URL")
	}
}
