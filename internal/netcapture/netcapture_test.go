package netcapture

import (
	"bytes"
	"strings"
	"testing"
)

func TestSessionRecordAndExport(t *testing.T) {
	s := NewSession("proj", "https://example.com")
	s.SetRecording(true)
	s.AddEntry(Entry{
		Request:  Req{Method: "GET", URL: "https://example.com/a", HTTPVersion: "HTTP/1.1"},
		Response: Resp{Status: 200, StatusText: "OK", HTTPVersion: "HTTP/1.1"},
		Time:     100,
	})
	s.AddEntry(Entry{
		Request:  Req{Method: "POST", URL: "https://example.com/b", HTTPVersion: "HTTP/1.1"},
		Response: Resp{Status: 201, StatusText: "Created", HTTPVersion: "HTTP/1.1"},
		Time:     50,
	})
	if len(s.Entries) != 2 {
		t.Fatalf("entries = %d", len(s.Entries))
	}
	// Not recording → entry dropped.
	s.SetRecording(false)
	s.AddEntry(Entry{Request: Req{URL: "https://example.com/c"}})
	if len(s.Entries) != 2 {
		t.Fatalf("entries after stop = %d", len(s.Entries))
	}
	var buf bytes.Buffer
	if err := s.WriteHAR(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"version": "1.2"`) {
		t.Fatalf("har version missing: %s", out)
	}
	if !strings.Contains(out, "https://example.com/a") {
		t.Fatalf("har entry missing: %s", out)
	}
}

func TestManagerOpenClose(t *testing.T) {
	m := NewManager()
	s := m.OpenSession("p1", "https://x.com")
	if s == nil {
		t.Fatal("nil session")
	}
	if _, ok := m.Session("p1"); !ok {
		t.Fatal("session not found")
	}
	if err := m.CloseSession("p1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Session("p1"); ok {
		t.Fatal("session should be closed")
	}
	if err := m.CloseSession("p1"); err == nil {
		t.Fatal("expected error closing missing session")
	}
}

func TestValidateURL(t *testing.T) {
	if err := ValidateURL(""); err == nil {
		t.Fatal("empty url should fail")
	}
	if err := ValidateURL("https://x.com"); err != nil {
		t.Fatal(err)
	}
}
