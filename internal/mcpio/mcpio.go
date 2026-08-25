// Package mcpio implements a minimal Model Context Protocol (MCP) server
// over newline-delimited JSON-RPC 2.0.
//
// Messages are one JSON object per line exchanged over an io.ReadWriter
// whose read and write directions are independent (os.Stdin+os.Stdout, a
// net.Conn or a pipe). The server speaks the MCP subset this repo uses:
// initialize, notifications/initialized, ping, tools/list and tools/call.
// Everything else is answered with -32601 (method not found) when the
// message carries an id, and consumed silently when it does not carry one.
package mcpio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ProtocolVersion is the MCP protocol version this server implements and
// advertises in the initialize response.
const ProtocolVersion = "2025-03-26"

// JSON-RPC 2.0 error codes (spec section 5.1).
const (
	CodeParseError     = -32700 // invalid JSON was received
	CodeInvalidRequest = -32600 // request object is not valid
	CodeMethodNotFound = -32601 // method does not exist
	CodeInvalidParams  = -32602 // method arguments are invalid
	CodeInternal       = -32603 // internal error (tool execution failures live here)
)

// ToolExecutionError is the message prefix for handler failures reported in
// the -32603 domain: it lets clients tell tool execution failures apart from
// transport-level internal errors.
const ToolExecutionError = "tool_execution_error"

// Tool is one MCP tool: a name, a description, a JSON Schema input contract
// and the handler that executes it. The handler result is JSON-marshaled
// into the response's text content; a handler error surfaces as a -32603
// error whose message carries the error text under the ToolExecutionError
// prefix.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, args map[string]any) (result any, err error)
}

// Server is a newline-delimited JSON-RPC 2.0 server bound to one
// io.ReadWriter. Register every tool before calling Serve; the tool set is
// treated as immutable while serving.
type Server struct {
	rw io.ReadWriter

	// Name and Version are echoed in the initialize response's serverInfo.
	Name    string
	Version string

	tools   map[string]*Tool
	ordered []string
}

// maxLineBytes bounds one incoming message; larger lines are answered with a
// parse error, drained and skipped.
const maxLineBytes = 16 << 20 // 16 MiB

// errLineTooLong marks a message exceeding maxLineBytes.
var errLineTooLong = errors.New("mcpio: message line too long")

// NewServer returns a server reading requests from and writing responses to
// rw.
func NewServer(rw io.ReadWriter) *Server {
	return &Server{
		rw:      rw,
		Name:    "mcp-server",
		Version: "dev",
		tools:   map[string]*Tool{},
	}
}

// RegisterTool adds t to the tool set. Empty names, missing handlers and
// duplicate names are rejected.
func (s *Server) RegisterTool(t Tool) error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("mcpio: tool name is empty")
	}
	if t.Handler == nil {
		return fmt.Errorf("mcpio: tool %q has no handler", t.Name)
	}
	if _, dup := s.tools[t.Name]; dup {
		return fmt.Errorf("mcpio: duplicate tool %q", t.Name)
	}
	s.tools[t.Name] = &t
	s.ordered = append(s.ordered, t.Name)
	return nil
}

// Serve reads and dispatches messages until the read side fails (io.EOF on a
// clean close), a write fails, or ctx is canceled. Cancellation returns
// ctx.Err(); a read blocked on the transport cannot be interrupted by the
// context, so callers that must stop promptly should also close the
// underlying reader.
func (s *Server) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() { done <- outcome{s.loop(ctx)} }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case o := <-done:
		return o.err
	}
}

// loop is the message pump: read one line, dispatch it, repeat.
func (s *Server) loop(ctx context.Context) error {
	br := bufio.NewReader(s.rw)
	for {
		line, err := readLine(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, errLineTooLong) {
				// readLine already drained the oversize line; report and
				// resync with the next message.
				if werr := s.writeError(nil, CodeParseError, "parse error: message line too long"); werr != nil {
					return werr
				}
				continue
			}
			return err
		}
		if err := s.handleLine(ctx, line); err != nil {
			return err
		}
	}
}

// readLine reads one newline-terminated line (the final line without a
// newline at EOF is also returned). Oversize lines are drained and reported
// as errLineTooLong.
func readLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		part, err := r.ReadSlice('\n')
		out = append(out, part...)
		switch {
		case err == nil:
			return out, nil
		case errors.Is(err, bufio.ErrBufferFull):
			if len(out) > maxLineBytes {
				drainOversize(r)
				return nil, errLineTooLong
			}
		case errors.Is(err, io.EOF):
			if len(out) == 0 {
				return nil, io.EOF
			}
			return out, nil
		default:
			return nil, err
		}
	}
}

// drainOversize consumes the remainder of an oversize line so the stream
// stays aligned on the next newline boundary.
func drainOversize(r *bufio.Reader) {
	for {
		_, err := r.ReadSlice('\n')
		if err == nil || errors.Is(err, io.EOF) {
			return
		}
		// bufio.ErrBufferFull: keep draining.
	}
}

// request is one decoded JSON-RPC message.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// handleLine validates and dispatches one message, returning the first write
// error (a broken transport stops the loop).
func (s *Server) handleLine(ctx context.Context, line []byte) error {
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil // blank separator lines are not messages
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return s.writeError(nil, CodeParseError, "parse error: "+err.Error())
	}
	// A missing or null id means notification semantics.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		// Protocol notifications are consumed silently...
		if strings.HasPrefix(req.Method, "notifications/") {
			return nil
		}
		// ...anything else without an id is not a valid request.
		return s.writeError(nil, CodeInvalidRequest, "invalid request: missing id")
	}
	if req.JSONRPC != "2.0" {
		return s.writeError(req.ID, CodeInvalidRequest, `invalid request: jsonrpc must be "2.0"`)
	}
	switch req.Method {
	case "initialize":
		return s.writeResult(req.ID, s.initializeResult())
	case "ping":
		return s.writeResult(req.ID, map[string]any{})
	case "tools/list":
		return s.writeResult(req.ID, map[string]any{"tools": s.toolsList()})
	case "tools/call":
		return s.handleCall(ctx, req)
	default:
		return s.writeError(req.ID, CodeMethodNotFound, fmt.Sprintf("method not found: %q", req.Method))
	}
}

// callParams is the tools/call request payload.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// handleCall runs one tool and reports the result per the MCP tools/call
// response shape.
func (s *Server) handleCall(ctx context.Context, req request) error {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		return s.writeError(req.ID, CodeInvalidParams, `invalid params: tools/call requires a non-empty "name"`)
	}
	t, ok := s.tools[p.Name]
	if !ok {
		return s.writeError(req.ID, CodeInvalidParams, fmt.Sprintf("invalid params: unknown tool %q", p.Name))
	}
	args := map[string]any{}
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return s.writeError(req.ID, CodeInvalidParams, `invalid params: "arguments" must be a JSON object`)
		}
	}
	result, err := t.Handler(ctx, args)
	if err != nil {
		return s.writeError(req.ID, CodeInternal, ToolExecutionError+": "+err.Error(), map[string]any{"tool": p.Name})
	}
	text, err := json.Marshal(result)
	if err != nil {
		return s.writeError(req.ID, CodeInternal, ToolExecutionError+": marshal result: "+err.Error(), map[string]any{"tool": p.Name})
	}
	return s.writeResult(req.ID, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(text)}},
		"isError": false,
	})
}

// initializeResult builds the response mandated by the MCP initialize
// handshake.
func (s *Server) initializeResult() map[string]any {
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{"name": s.Name, "version": s.Version},
	}
}

// toolsList renders the registered tools in registration order.
func (s *Server) toolsList() []any {
	out := make([]any, 0, len(s.ordered))
	for _, name := range s.ordered {
		t := s.tools[name]
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{}
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
		})
	}
	return out
}

// rpcError is the JSON-RPC error object {code, message, data}.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// response is one outgoing JSON-RPC message.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) writeResult(id json.RawMessage, result any) error {
	return s.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeError(id json.RawMessage, code int, message string, data ...any) error {
	e := &rpcError{Code: code, Message: message}
	if len(data) > 0 && data[0] != nil {
		e.Data = data[0]
	}
	return s.write(response{JSONRPC: "2.0", ID: id, Error: e})
}

func (s *Server) write(resp response) error {
	return json.NewEncoder(s.rw).Encode(resp)
}
