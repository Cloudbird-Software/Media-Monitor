package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// cursorPlatform serves two 20-item comment pages keyed by the cursor query
// param and records the cursor values it received (proves the engine got
// the caller's cursor instead of restarting).
type cursorPlatform struct {
	srv      *httptest.Server
	mu       sync.Mutex
	received []string
}

func newCursorPlatform(t *testing.T) *cursorPlatform {
	t.Helper()
	cp := &cursorPlatform{}
	page := func(prefix, next string, hasMore bool) string {
		body := `{"comments":[`
		for i := 0; i < 20; i++ {
			if i > 0 {
				body += ","
			}
			body += fmt.Sprintf(`{"cid":%q,"text":"t%d"}`, prefix+"-"+fmt.Sprint(i), i)
		}
		body += fmt.Sprintf(`],"has_more":%v,"cursor":%q}`, hasMore, next)
		return body
	}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := r.URL.Query().Get("cursor")
		cp.mu.Lock()
		cp.received = append(cp.received, cur)
		cp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if cur == "" {
			fmt.Fprint(w, page("p1", "p2", true))
			return
		}
		fmt.Fprint(w, page("p2", "", false))
	}))
	return cp
}

// writeCursorAdaptDir lays out a douyin comments contract pointing at srv.
func writeCursorAdaptDir(t *testing.T, srvURL string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "contracts"), 0o755); err != nil {
		t.Fatal(err)
	}
	contract := fmt.Sprintf(`{
	  "name": "douyin-comments", "platform": "douyin", "category": "comments", "version": "1",
	  "doc": "test-only cursor contract",
	  "transport": {"base_url": %q, "path": "/cmt/", "method": "GET", "placeholders": ["item_id"]},
	  "binding": {"comments": "$.comments"},
	  "paging": {"cursor_param": "cursor", "count_param": "count", "count_default": 20, "has_more_path": "$.has_more", "next_cursor_path": "$.cursor"}
	}`, srvURL)
	if err := os.WriteFile(filepath.Join(dir, "contracts", "douyin-comments.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCursorPassthroughChain: get_comments without a cursor serves page 1;
// feeding the returned next_cursor back serves page 2 through the same
// engine call — the platform receives the caller's cursor (the handler no
// longer discards it) and the two calls yield 2×limit distinct comments.
// Fail-before: with model.Cursor{} hardcoded the second call re-served
// page 1.
func TestCursorPassthroughChain(t *testing.T) {
	cp := newCursorPlatform(t)
	defer cp.srv.Close()
	t.Setenv("MEDIAMON_ADAPT_DIR", writeCursorAdaptDir(t, cp.srv.URL))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c := startServer(t)

	first := c.callTool(t, "get_comments", map[string]any{"platform": "douyin", "item_id": "i1", "limit": 20})
	if first == nil {
		t.Fatal("first call failed")
	}
	next, ok := first["next_cursor"].(map[string]any)
	if !ok {
		t.Fatalf("first call missing next_cursor: %v", first)
	}
	if next["v"] != float64(1) {
		t.Fatalf("next_cursor.v = %v, want 1", next["v"])
	}
	src, _ := next["source"].(map[string]any)
	if src["cursor"] != "p2" {
		t.Fatalf("next_cursor.source.cursor = %v, want p2", src["cursor"])
	}

	second := c.callTool(t, "get_comments", map[string]any{"platform": "douyin", "item_id": "i1", "limit": 20, "cursor": next})
	if second == nil {
		t.Fatal("second call failed")
	}
	cp.mu.Lock()
	got := append([]string(nil), cp.received...)
	cp.mu.Unlock()
	if len(got) != 2 || got[0] != "" || got[1] != "p2" {
		t.Fatalf("platform cursors = %v, want [\"\", p2] (engine received the caller's cursor)", got)
	}
}

// TestCursorOmittedBehavesAsBefore: omitting the cursor is a fresh first
// page (backward compatible), and a foreign envelope version is an explicit
// error instead of silent misbehavior.
func TestCursorOmittedBehavesAsBefore(t *testing.T) {
	cp := newCursorPlatform(t)
	defer cp.srv.Close()
	t.Setenv("MEDIAMON_ADAPT_DIR", writeCursorAdaptDir(t, cp.srv.URL))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c := startServer(t)

	// omitted cursor: first page, twice (no hidden state).
	for i := 0; i < 2; i++ {
		if out := c.callTool(t, "get_comments", map[string]any{"platform": "douyin", "item_id": "i1", "limit": 20}); out == nil {
			t.Fatal("cursorless call failed")
		}
	}
	cp.mu.Lock()
	got := append([]string(nil), cp.received...)
	cp.mu.Unlock()
	if len(got) != 2 || got[0] != "" || got[1] != "" {
		t.Fatalf("platform cursors = %v, want two fresh first pages", got)
	}

	msg := c.callToolErr(t, "get_comments", map[string]any{"platform": "douyin", "item_id": "i1", "cursor": map[string]any{"v": 99}})
	if msg == "" || !strings.Contains(msg, "unsupported") {
		t.Fatalf("version rejection = %q, want explicit unsupported-version error", msg)
	}
}
