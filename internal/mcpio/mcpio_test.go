package mcpio_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/mcpio"
)

// testClient is the test side of a net.Pipe full-duplex link. A dedicated
// goroutine decodes every response line so reads never race and timeouts are
// applied per receive.
type testClient struct {
	conn  net.Conn
	lines chan map[string]any
}

// start opens a fresh pipe, binds a server to its server side with the given
// tools registered, launches Serve in the background and returns the client
// side.
func start(t *testing.T, ctx context.Context, tools []mcpio.Tool) *testClient {
	t.Helper()
	c, s := net.Pipe()
	srv := mcpio.NewServer(s)
	srv.Name = "test-server"
	srv.Version = "9.9.9"
	for _, tl := range tools {
		if err := srv.RegisterTool(tl); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	go func() { _ = srv.Serve(ctx) }()
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
	return &testClient{conn: c, lines: lines}
}

func (c *testClient) send(t *testing.T, raw string) {
	t.Helper()
	if _, err := c.conn.Write([]byte(raw + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (c *testClient) recv(t *testing.T) map[string]any {
	t.Helper()
	select {
	case m := <-c.lines:
		if m == nil {
			t.Fatalf("connection closed before a response arrived")
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for a response")
		return nil
	}
}

func (c *testClient) close() { c.conn.Close() }

// newID renders the id argument as a JSON scalar (string, number or null).
func newID(id any) string {
	switch v := id.(type) {
	case string:
		b, _ := json.Marshal(v)
		return string(b)
	case int:
		return fmt.Sprintf("%d", v)
	case nil:
		return "null"
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// build renders one request/notification line.
func build(method string, id any, params map[string]any) string {
	var b strings.Builder
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.WriteString(newID(id))
	b.WriteString(`,"method":`)
	mb, _ := json.Marshal(method)
	b.WriteString(string(mb))
	if params != nil {
		pb, _ := json.Marshal(params)
		b.WriteString(`,"params":`)
		b.WriteString(string(pb))
	}
	b.WriteString("}")
	return b.String()
}

// buildNoID renders a message without an id field (notification shape).
func buildNoID(method string) string {
	mb, _ := json.Marshal(method)
	return `{"jsonrpc":"2.0","method":` + string(mb) + `}`
}

// testTools returns the echo/boom/weird tool set used by most tests.
func testTools() []mcpio.Tool {
	return []mcpio.Tool{
		{
			Name:        "echo",
			Description: "echoes its arguments",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler: func(_ context.Context, args map[string]any) (any, error) {
				return map[string]any{"args": args}, nil
			},
		},
		{
			Name:        "boom",
			Description: "always fails",
			InputSchema: map[string]any{"type": "object"},
			Handler: func(_ context.Context, _ map[string]any) (any, error) {
				return nil, fmt.Errorf("kaboom")
			},
		},
		{
			Name:        "weird",
			Description: "returns an unmarshalable result",
			InputSchema: map[string]any{},
			Handler: func(_ context.Context, _ map[string]any) (any, error) {
				return func() {}, nil
			},
		},
	}
}

func wantError(t *testing.T, m map[string]any, code float64, msgContains string) {
	t.Helper()
	if m["result"] != nil {
		t.Fatalf("unexpected result: %v", m["result"])
	}
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object: %v", m)
	}
	if e["code"] != code {
		t.Fatalf("error code = %v, want %v (message %v)", e["code"], code, e["message"])
	}
	if msgContains != "" && !strings.Contains(fmt.Sprint(e["message"]), msgContains) {
		t.Fatalf("error message %q does not contain %q", e["message"], msgContains)
	}
}

func TestInitialize(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, build("initialize", "1", map[string]any{"protocolVersion": "2024-11-05"}))
	m := c.recv(t)
	if m["id"] != "1" {
		t.Fatalf("id = %v", m["id"])
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	if res["protocolVersion"] != mcpio.ProtocolVersion {
		t.Fatalf("protocolVersion = %v", res["protocolVersion"])
	}
	caps, ok := res["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("no capabilities: %v", res)
	}
	tools, ok := caps["tools"].(map[string]any)
	if !ok || tools["listChanged"] != false {
		t.Fatalf("capabilities.tools = %v", caps["tools"])
	}
	info, ok := res["serverInfo"].(map[string]any)
	if !ok || info["name"] != "test-server" || info["version"] != "9.9.9" {
		t.Fatalf("serverInfo = %v", res["serverInfo"])
	}
}

func TestPing(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, build("ping", 42, nil))
	m := c.recv(t)
	if m["id"] != float64(42) {
		t.Fatalf("id = %v", m["id"])
	}
	if got := fmt.Sprint(m["result"]); got != "map[]" {
		t.Fatalf("ping result = %v", got)
	}
}

func TestToolsList(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, build("tools/list", "id-1", nil))
	m := c.recv(t)
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	list, ok := res["tools"].([]any)
	if !ok || len(list) != 3 {
		t.Fatalf("tools = %v", res["tools"])
	}
	first, ok := list[0].(map[string]any)
	if !ok || first["name"] != "echo" || first["description"] != "echoes its arguments" {
		t.Fatalf("first tool = %v", list[0])
	}
	if _, ok := first["inputSchema"].(map[string]any); !ok {
		t.Fatalf("inputSchema missing: %v", first)
	}
}

func TestToolCallSuccess(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, build("tools/call", "t1", map[string]any{"name": "echo", "arguments": map[string]any{"a": 1}}))
	m := c.recv(t)
	if m["id"] != "t1" {
		t.Fatalf("id = %v", m["id"])
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	if res["isError"] != false {
		t.Fatalf("isError = %v", res["isError"])
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %v", res["content"])
	}
	text, ok := content[0].(map[string]any)["text"].(string)
	if !ok {
		t.Fatalf("content[0] = %v", content[0])
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("result text is not JSON: %v", err)
	}
	args, ok := got["args"].(map[string]any)
	if !ok || args["a"] != float64(1) {
		t.Fatalf("result text = %q", text)
	}
}

func TestToolCallNoArguments(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	for _, params := range []map[string]any{
		{"name": "echo"},
		{"name": "echo", "arguments": map[string]any{}},
		{"name": "echo", "arguments": nil},
	} {
		c.send(t, build("tools/call", "x", params))
		m := c.recv(t)
		res, ok := m["result"].(map[string]any)
		if !ok {
			t.Fatalf("no result for %v: %v", params, m)
		}
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(text, `"args":{}`) {
			t.Fatalf("text = %q", text)
		}
	}
}

func TestToolCallHandlerError(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, build("tools/call", "e1", map[string]any{"name": "boom", "arguments": map[string]any{}}))
	m := c.recv(t)
	if m["id"] != "e1" {
		t.Fatalf("id = %v", m["id"])
	}
	wantError(t, m, -32603, "tool_execution_error: kaboom")
	e := m["error"].(map[string]any)
	data, ok := e["data"].(map[string]any)
	if !ok || data["tool"] != "boom" {
		t.Fatalf("error data = %v", e["data"])
	}
}

func TestToolCallMarshalError(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, build("tools/call", "m1", map[string]any{"name": "weird"}))
	m := c.recv(t)
	wantError(t, m, -32603, "marshal result")
}

func TestToolCallUnknownTool(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, build("tools/call", 7, map[string]any{"name": "nope"}))
	m := c.recv(t)
	if m["id"] != float64(7) {
		t.Fatalf("id = %v", m["id"])
	}
	wantError(t, m, -32602, "unknown tool \"nope\"")
}

func TestToolCallMissingAndBadParams(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	cases := []struct{ line, want string }{
		{build("tools/call", "a", map[string]any{"arguments": map[string]any{}}), "requires a non-empty \"name\""},
		{build("tools/call", "b", nil), "requires a non-empty \"name\""},
		{build("tools/call", "c", map[string]any{"name": "echo", "arguments": 42}), "\"arguments\" must be a JSON object"},
		{build("tools/call", "d", map[string]any{"name": "echo", "arguments": []any{1}}), "\"arguments\" must be a JSON object"},
	}
	for _, tc := range cases {
		c.send(t, tc.line)
		m := c.recv(t)
		wantError(t, m, -32602, tc.want)
	}
}

func TestUnknownMethod(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, build("frobnicate", "u1", nil))
	m := c.recv(t)
	wantError(t, m, -32601, `method not found: "frobnicate"`)
}

func TestParseError(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, `{"jsonrpc":"2.0","id":1,`)
	m := c.recv(t)
	if m["id"] != nil {
		t.Fatalf("parse error id = %v, want null", m["id"])
	}
	wantError(t, m, -32700, "parse error")
}

func TestBlankLineIgnored(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	for _, blank := range []string{"", "   ", "\t\n"} {
		c.send(t, blank)
	}
	// No response may have been sent for the blanks; the ping response is the
	// first thing that must arrive.
	c.send(t, build("ping", "after-blank", nil))
	m := c.recv(t)
	if m["id"] != "after-blank" {
		t.Fatalf("id = %v", m["id"])
	}
}

func TestOversizeLineParseErrorAndResync(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	big := strings.Repeat("x", 17<<20)
	c.send(t, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"`+big+`"}}`)
	m := c.recv(t)
	if m["id"] != nil {
		t.Fatalf("oversize line id = %v, want null", m["id"])
	}
	wantError(t, m, -32700, "line too long")
	// The stream must be resynchronized after the oversize line.
	c.send(t, build("ping", "resync", nil))
	m = c.recv(t)
	if m["id"] != "resync" {
		t.Fatalf("id = %v", m["id"])
	}
}

func TestInvalidJSONRPCVersion(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, `{"jsonrpc":"1.0","id":"v","method":"ping"}`)
	m := c.recv(t)
	if m["id"] != "v" {
		t.Fatalf("id = %v", m["id"])
	}
	wantError(t, m, -32600, "jsonrpc")
}

func TestMissingID(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	// tools/list without an id is not a valid request (it is not a protocol
	// notification), so it is answered with -32600.
	c.send(t, buildNoID("tools/list"))
	m := c.recv(t)
	if m["id"] != nil {
		t.Fatalf("id = %v, want null", m["id"])
	}
	wantError(t, m, -32600, "missing id")
	// An empty message without method or id is equally invalid.
	c.send(t, `{"jsonrpc":"2.0"}`)
	m = c.recv(t)
	wantError(t, m, -32600, "missing id")
}

func TestNotificationsConsumedSilently(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	c.send(t, buildNoID("notifications/initialized"))
	c.send(t, buildNoID("notifications/tools/list_changed"))
	c.send(t, buildNoID("notifications/unknown/branch"))
	// None of the notifications may produce a response; the ping response
	// proves the pipe stayed clean.
	c.send(t, build("ping", "after-notif", nil))
	m := c.recv(t)
	if m["id"] != "after-notif" {
		t.Fatalf("id = %v", m["id"])
	}
}

func TestSequentialCallsMatchOrder(t *testing.T) {
	c := start(t, context.Background(), testTools())
	defer c.close()
	for i := 0; i < 10; i++ {
		c.send(t, build("tools/call", fmt.Sprintf("seq-%d", i), map[string]any{"name": "echo", "arguments": map[string]any{"i": i}}))
	}
	for i := 0; i < 10; i++ {
		m := c.recv(t)
		if m["id"] != fmt.Sprintf("seq-%d", i) {
			t.Fatalf("call %d: id = %v", i, m["id"])
		}
	}
}

func TestRegisterToolValidation(t *testing.T) {
	srv := mcpio.NewServer(nil)
	if err := srv.RegisterTool(mcpio.Tool{Name: "", Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }}); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := srv.RegisterTool(mcpio.Tool{Name: "x"}); err == nil {
		t.Fatal("nil handler accepted")
	}
	ok := mcpio.Tool{Name: "x", Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }}
	if err := srv.RegisterTool(ok); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := srv.RegisterTool(ok); err == nil {
		t.Fatal("duplicate name accepted")
	}
}

func TestServeReturnsEOF(t *testing.T) {
	c, s := net.Pipe()
	srv := mcpio.NewServer(s)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background()) }()
	c.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
}

func TestServeContextCancel(t *testing.T) {
	c, s := net.Pipe()
	srv := mcpio.NewServer(s)
	defer s.Close()
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Serve = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestHandlerReceivesServeContext(t *testing.T) {
	ctxCh := make(chan context.Context, 1)
	ctx, cancel := context.WithCancel(context.Background())
	c := start(t, ctx, []mcpio.Tool{{
		Name: "ctxprobe",
		Handler: func(hctx context.Context, _ map[string]any) (any, error) {
			ctxCh <- hctx
			return map[string]any{"ok": true}, nil
		},
	}})
	defer c.close()
	c.send(t, build("tools/call", "q", map[string]any{"name": "ctxprobe"}))
	m := c.recv(t)
	if m["error"] != nil {
		t.Fatalf("error: %v", m["error"])
	}
	select {
	case got := <-ctxCh:
		cancel()
		select {
		case <-got.Done():
		default:
			t.Fatal("handler context is not derived from the Serve context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}
}
