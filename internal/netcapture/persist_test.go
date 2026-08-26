package netcapture

import (
	"strings"
	"testing"
)

// TestPersistentRoundTrip: entries recorded through one manager (one
// "process") are readable by a fresh manager over the same directory —
// the cross-process read path that previously always reported "no session".
func TestPersistentRoundTrip(t *testing.T) {
	dir := t.TempDir()

	m1, err := NewManagerDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := m1.OpenSession("proj", "https://example.com")
	if s == nil {
		t.Fatal("persistent OpenSession returned nil")
	}
	s.SetRecording(true)
	s.AddEntry(Entry{
		Request:  Req{Method: "GET", URL: "https://example.com/a", HTTPVersion: "HTTP/1.1"},
		Response: Resp{Status: 200, StatusText: "OK", HTTPVersion: "HTTP/1.1"},
		Time:     42,
	})
	if err := m1.Close(); err != nil {
		t.Fatal(err)
	}

	// Fresh manager = a new process over the same data directory.
	m2, err := NewManagerDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if _, ok := m2.Session("proj"); ok {
		t.Fatal("fresh manager must not hold the session in memory")
	}
	loaded, ok, err := m2.Load("proj")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatal("persisted session not found by fresh manager")
	}
	if loaded.URL != "https://example.com" {
		t.Fatalf("url = %q", loaded.URL)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Request.URL != "https://example.com/a" {
		t.Fatalf("entries = %+v", loaded.Entries)
	}
	names, err := m2.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "proj" {
		t.Fatalf("List = %v, want [proj]", names)
	}

	// Re-opening the session on the fresh manager accumulates entries.
	s2 := m2.OpenSession("proj", "https://example.com")
	if s2 == nil {
		t.Fatal("persistent re-open returned nil")
	}
	if len(s2.Entries) != 1 {
		t.Fatalf("reopened entries = %d, want 1 (reloaded)", len(s2.Entries))
	}
	s2.SetRecording(true)
	s2.AddEntry(Entry{Request: Req{Method: "GET", URL: "https://example.com/b"}})
	loaded2, ok, err := m2.Load("proj")
	if err != nil || !ok {
		t.Fatalf("Load2: ok=%v err=%v", ok, err)
	}
	if len(loaded2.Entries) != 2 {
		t.Fatalf("entries after reopen = %d, want 2 (accumulated)", len(loaded2.Entries))
	}
}

// TestPersistentRejectsUnsafeName: project names that are not safe store
// segments fail closed (nil session + error from OpenSessionE).
func TestPersistentRejectsUnsafeName(t *testing.T) {
	m, err := NewManagerDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if s := m.OpenSession("bad/name", ""); s != nil {
		t.Fatal("unsafe project name must fail closed")
	}
	if _, err := m.OpenSessionE("bad/name", ""); err == nil {
		t.Fatal("expected error for unsafe project name")
	}
}

// TestLoadUnknownAndNonPersistent: Load reports "not found" for unknown
// sessions and errors on in-memory managers.
func TestLoadUnknownAndNonPersistent(t *testing.T) {
	m, err := NewManagerDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, ok, err := m.Load("nope"); err != nil || ok {
		t.Fatalf("Load unknown: ok=%v err=%v, want false/nil", ok, err)
	}
	if _, _, err := NewManager().Load("x"); err == nil || !strings.Contains(err.Error(), "not persistent") {
		t.Fatalf("expected non-persistent error, got %v", err)
	}
	names, err := NewManager().List()
	if err != nil || len(names) != 0 {
		t.Fatalf("in-memory List = %v, %v", names, err)
	}
}
