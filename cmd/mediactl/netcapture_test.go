package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/netcapture"
)

// writePersistedSession records one entry into the persistent netcapture
// store, simulating a session written by an earlier process (mediad).
func writePersistedSession(t *testing.T, dir, project string) {
	t.Helper()
	mgr, err := netcapture.NewManagerDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := mgr.OpenSessionE(project, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	s.SetRecording(true)
	s.AddEntry(netcapture.Entry{
		Request:  netcapture.Req{Method: "GET", URL: "https://example.com/a", HTTPVersion: "HTTP/1.1"},
		Response: netcapture.Resp{Status: 200, StatusText: "OK", HTTPVersion: "HTTP/1.1"},
	})
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNetcaptureListAndExport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "netcapture")
	t.Setenv("MEDIAMON_NETCAPTURE_DIR", dir)
	writePersistedSession(t, dir, "proj1")

	// list shows the persisted session with its entry count (cross-process
	// read: this manager instance never recorded anything itself).
	out, err := captureStdout(t, func() error {
		return cmdNetcapture([]string{"list"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "proj1\tentries=1") {
		t.Fatalf("list output = %q", out)
	}

	// export writes a standards-shaped HAR with the persisted entry.
	har := filepath.Join(t.TempDir(), "out.har")
	out, err = captureStdout(t, func() error {
		return cmdNetcapture([]string{"export", "--project", "proj1", "--out", har})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `exported session "proj1" (1 entries)`) {
		t.Fatalf("export output = %q", out)
	}
	raw, err := os.ReadFile(har)
	if err != nil {
		t.Fatal(err)
	}
	var doc netcapture.HAR
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("HAR is not valid JSON: %v", err)
	}
	if doc.Log.Version != "1.2" || len(doc.Log.Entries) != 1 {
		t.Fatalf("HAR = %+v", doc.Log)
	}
	if doc.Log.Entries[0].Request.URL != "https://example.com/a" || doc.Log.Entries[0].Response.Status != 200 {
		t.Fatalf("HAR entry = %+v", doc.Log.Entries[0])
	}
}

func TestNetcaptureExportMissingSession(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "netcapture")
	t.Setenv("MEDIAMON_NETCAPTURE_DIR", dir)
	writePersistedSession(t, dir, "proj1")

	err := cmdNetcapture([]string{"export", "--project", "ghost", "--out", filepath.Join(t.TempDir(), "x.har")})
	if err == nil || !strings.Contains(err.Error(), `no session "ghost"`) {
		t.Fatalf("error = %v", err)
	}
	if err := cmdNetcapture([]string{"export", "--project", "proj1"}); err == nil {
		t.Fatal("missing --out accepted")
	}
	if err := cmdNetcapture([]string{"record"}); err == nil {
		t.Fatal("record subcommand should not exist (no CDP capture in this CLI)")
	}
}
