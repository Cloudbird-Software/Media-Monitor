package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testClient is the MCP client side of a net.Pipe speaking newline-delimited
// JSON-RPC 2.0.
type testClient struct {
	conn  net.Conn
	lines chan map[string]any
}

// startServer launches run() (the full stdio server) on one end of a fresh
// pipe. run reads its configuration from the process environment, so tests
// must set env vars (t.Setenv) before calling.
func startServer(t *testing.T) *testClient {
	t.Helper()
	c, s := net.Pipe()
	go func() { _ = run(s) }()
	lines := make(chan map[string]any, 32)
	go func() {
		dec := json.NewDecoder(c)
		for {
			var m map[string]any
			if err := dec.Decode(&m); err != nil {
				lines <- nil
				return
			}
			lines <- m
		}
	}()
	t.Cleanup(func() { c.Close() })
	return &testClient{conn: c, lines: lines}
}

func (c *testClient) call(t *testing.T, method string, id string, params map[string]any) map[string]any {
	t.Helper()
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case resp := <-c.lines:
		if resp == nil {
			t.Fatalf("connection closed before a response arrived")
		}
		return resp
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for response to %s", method)
		return nil
	}
}

// callOK runs a tools/call and returns the decoded result object.
func (c *testClient) callTool(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	m := c.call(t, "tools/call", "id-"+name, map[string]any{"name": name, "arguments": args})
	if m["error"] != nil {
		t.Fatalf("tool %s error: %v", name, m["error"])
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("tool %s: no result: %v", name, m)
	}
	text, ok := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !ok {
		t.Fatalf("tool %s: no text content: %v", name, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("tool %s: result %q is not JSON: %v", name, text, err)
	}
	return out
}

// callToolErr runs a tools/call expecting an error and returns its message.
func (c *testClient) callToolErr(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	m := c.call(t, "tools/call", "err-"+name, map[string]any{"name": name, "arguments": args})
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("tool %s: expected an error, got %v", name, m)
	}
	if e["code"] != float64(-32603) {
		t.Fatalf("tool %s: error code = %v, want -32603", name, e["code"])
	}
	return fmt.Sprint(e["message"])
}

// writeAdaptDir lays out a minimal adapt tree (contract + fixture + canary)
// sufficient for the collect engine and the offline canary harness.
func writeAdaptDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("contracts/demo-canary.json", `{
	  "name": "demo-canary",
	  "platform": "generic",
	  "category": "search",
	  "version": "1",
	  "doc": "test-only contract",
	  "transport": {"base_url": "https://example.test", "path": "/x", "method": "GET"},
	  "binding": {"items": "$.data"}
	}`)
	// douyin-comments-replies is the declared douyin replies contract; it
	// requires a_bogus, so an unsigned run fails closed on the signature gate.
	mustWrite("contracts/douyin-comments-replies.json", `{
	  "name": "douyin-comments-replies",
	  "platform": "douyin",
	  "category": "replies",
	  "version": "1",
	  "doc": "test-only douyin replies contract",
	  "transport": {"base_url": "https://example.test", "path": "/reply", "method": "GET", "placeholders": ["comment_id"]},
	  "signature": {"params": ["a_bogus"], "required": ["a_bogus"]},
	  "binding": {"comments": "$.comments"}
	}`)
	mustWrite("fixtures/fixture.json", `{"data": [{"id": "1", "desc": "first"}]}`)
	mustWrite("canaries/one.json", `{"canaries": [{"name": "test-canary", "contract": "demo-canary", "kind": "items", "fixture": "fixture.json", "expect": ["data"]}]}`)
	return dir
}

func TestServerToolsAndBasics(t *testing.T) {
	t.Setenv("MEDIAMON_ADAPT_DIR", writeAdaptDir(t))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c := startServer(t)

	m := c.call(t, "initialize", "i1", map[string]any{"protocolVersion": "2025-03-26"})
	res := m["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" {
		t.Fatalf("protocolVersion = %v", res["protocolVersion"])
	}

	m = c.call(t, "tools/list", "l1", nil)
	toolList := m["result"].(map[string]any)["tools"].([]any)
	if len(toolList) != 21 {
		t.Fatalf("tool count = %d, want 21", len(toolList))
	}
	names := map[string]bool{}
	for _, tl := range toolList {
		obj := tl.(map[string]any)
		names[obj["name"].(string)] = true
		schema, ok := obj["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("tool %v: inputSchema not JSON-Schema-style: %v", obj["name"], obj["inputSchema"])
		}
		if _, ok := schema["properties"]; !ok {
			t.Fatalf("tool %v: inputSchema has no properties", obj["name"])
		}
	}
	for _, want := range []string{
		"search_items", "get_comments", "get_replies", "get_user", "group_members",
		"resolve_video", "get_collects", "get_im_unread",
		"monitor_live", "read_live_events", "submit_task", "list_tasks",
		"adapt_canary_offline", "contracts_list", "send_message", "accounts_list",
		"adb_list", "adb_shell", "adb_screencap", "version",
	} {
		if !names[want] {
			t.Fatalf("tool %q not registered", want)
		}
	}

	out := c.callTool(t, "version", map[string]any{})
	if out["name"] != "mediad-mcp" || out["version"] == "" {
		t.Fatalf("version = %v", out)
	}

	out = c.callTool(t, "contracts_list", map[string]any{})
	found := false
	for _, cn := range out["contracts"].([]any) {
		if cn == "demo-canary" {
			found = true
		}
	}
	if !found {
		t.Fatalf("contracts_list = %v", out["contracts"])
	}

	out = c.callTool(t, "adapt_canary_offline", map[string]any{})
	if out["healthy"] != true || out["cases"] != float64(1) {
		t.Fatalf("canary = %v", out)
	}
	if !strings.Contains(fmt.Sprint(out["report"]), "demo-canary: healthy") {
		t.Fatalf("canary report = %v", out["report"])
	}

	// Unknown / structurally invalid tool calls surface as tool errors.
	msg := c.callToolErr(t, "search_items", map[string]any{"platform": "douyin"})
	if !strings.Contains(msg, "keyword is required") {
		t.Fatalf("search_items error = %q", msg)
	}
	// douyin now declares a replies contract; without a signer the fetch fails
	// closed on the signature gate (a_bogus) — the contract is real, not a
	// missing-declaration placeholder.
	msg = c.callToolErr(t, "get_replies", map[string]any{"platform": "douyin", "item_id": "i", "cid": "c"})
	if !strings.Contains(msg, "a_bogus") {
		t.Fatalf("get_replies error = %q, want signature-required (a_bogus)", msg)
	}
}

func TestTasksTools(t *testing.T) {
	t.Setenv("MEDIAMON_ADAPT_DIR", writeAdaptDir(t))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c := startServer(t)

	out := c.callTool(t, "list_tasks", map[string]any{})
	if n := len(out["tasks"].([]any)); n != 0 {
		t.Fatalf("list_tasks on empty store = %d tasks", n)
	}

	out = c.callTool(t, "submit_task", map[string]any{"kind": "search", "config": map[string]any{"kw": "x"}})
	if out["kind"] != "search" || out["state"] != "queued" {
		t.Fatalf("task = %v", out)
	}
	taskID := fmt.Sprint(out["id"])

	out = c.callTool(t, "list_tasks", map[string]any{})
	tasks := out["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("list_tasks = %v", out["tasks"])
	}
	if tasks[0].(map[string]any)["id"] != taskID {
		t.Fatalf("task id mismatch: %v", tasks[0])
	}

	msg := c.callToolErr(t, "submit_task", map[string]any{})
	if !strings.Contains(msg, "kind is required") {
		t.Fatalf("submit_task error = %q", msg)
	}
}

func TestMonitorLiveRequiresSigner(t *testing.T) {
	t.Setenv("MEDIAMON_ADAPT_DIR", writeAdaptDir(t))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c := startServer(t)

	msg := c.callToolErr(t, "monitor_live", map[string]any{
		"room_url":       "https://live.douyin.com/12345",
		"allow_unsigned": false,
	})
	if !strings.Contains(msg, "no signature signer configured") || !strings.Contains(msg, "MEDIAMON_SIGNER_URL") {
		t.Fatalf("monitor_live error = %q", msg)
	}
}

func TestMonitorLiveAllowUnsignedSession(t *testing.T) {
	t.Setenv("MEDIAMON_ADAPT_DIR", writeAdaptDir(t))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	// Redirect every live-network hop to a dead local endpoint so the session
	// fails fast with zero external traffic.
	t.Setenv("MEDIAMON_LIVE_PAGE_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("MEDIAMON_LIVE_WSS_ENDPOINT", "http://127.0.0.1:1")
	c := startServer(t)

	out := c.callTool(t, "monitor_live", map[string]any{
		"room_url":       "https://live.douyin.com/12345",
		"allow_unsigned": true,
	})
	lobbyID := fmt.Sprint(out["session_id"])
	if !strings.HasPrefix(lobbyID, "lobby-") {
		t.Fatalf("session_id = %q", lobbyID)
	}
	if _, ok := out["room_id"]; !ok {
		t.Fatalf("monitor_live result lacks room_id: %v", out)
	}

	// The session goroutine dials the dead endpoint and records the failure;
	// poll read_live_events until the session reports ended.
	deadline := time.Now().Add(10 * time.Second)
	for {
		out = c.callTool(t, "read_live_events", map[string]any{"lobby_id": lobbyID})
		if out["ended"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never ended: %v", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out["end_error"] == nil || fmt.Sprint(out["end_error"]) == "" {
		t.Fatalf("session ended without an error: %v", out)
	}
	if out["next"] != float64(0) {
		t.Fatalf("next = %v", out["next"])
	}

	msg := c.callToolErr(t, "read_live_events", map[string]any{"lobby_id": "lobby-nope"})
	if !strings.Contains(msg, `lobby_id "lobby-nope"`) {
		t.Fatalf("read_live_events error = %q", msg)
	}
	msg = c.callToolErr(t, "read_live_events", map[string]any{"lobby_id": lobbyID, "after": -1})
	if !strings.Contains(msg, "after must be >= 0") {
		t.Fatalf("read_live_events after error = %q", msg)
	}
}

func TestRunFailsWithoutAdaptDir(t *testing.T) {
	t.Setenv("MEDIAMON_ADAPT_DIR", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c, s := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- run(s) }()
	// run must fail fast with a contracts error before any message exchange.
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "contracts") {
			t.Fatalf("run = %v, want contracts error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not fail without an adapt dir")
	}
	c.Close()
}
