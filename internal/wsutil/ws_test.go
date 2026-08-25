package wsutil

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

// -------- minimal RFC 6455 test server built on http.Hijacker --------

// wsTestServer bundles a hijacked test server with its expected headers.
type wsTestServer struct {
	srv        *httptest.Server
	sawCloseCh chan bool // "observe" mode reports whether a close frame arrived
	wantHeader http.Header
}

// wsHandler builds the hijacked RFC 6455 server handler described by mode
// (see serveWS) with handshake header assertions against wantHeader.
func wsHandler(t *testing.T, mode string, wantHeader http.Header, sawCloseCh chan bool) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hj, ok := rw.(http.Hijacker)
		if !ok {
			t.Error("response writer does not support hijacking")
			return
		}
		raw, brw, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer raw.Close()

		// net/http has already consumed the request headers before the
		// handler ran; after Hijack the request must be read from `r`,
		// not from a fresh http.ReadRequest (which would block forever —
		// the header bytes are already gone from the wire).
		req := r
		if req.Method != http.MethodGet {
			t.Errorf("handshake method = %s, want GET", req.Method)
		}
		if req.Header.Get("Upgrade") != "websocket" {
			t.Errorf("Upgrade = %q", req.Header.Get("Upgrade"))
		}
		if !headerContains(req.Header, "Connection", "Upgrade") {
			t.Errorf("Connection header does not claim Upgrade")
		}
		if req.Header.Get("Sec-WebSocket-Version") != "13" {
			t.Errorf("Sec-WebSocket-Version = %q", req.Header.Get("Sec-WebSocket-Version"))
		}
		key := req.Header.Get("Sec-WebSocket-Key")
		if key == "" {
			t.Error("missing Sec-WebSocket-Key")
		}
		for k, vs := range wantHeader {
			if got := req.Header.Get(k); got != vs[0] {
				t.Errorf("header %s = %q, want %q", k, got, vs[0])
			}
		}
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", acceptKey(key))
		brw.Flush()

		serveWS(t, brw, mode, sawCloseCh)
	})
}

// newWSTestServer starts an httptest server whose handler performs the WS
// handshake (with assertions) and then runs the given wire mode.
func newWSTestServer(t *testing.T, mode string, wantHeader http.Header) *wsTestServer {
	t.Helper()
	w := &wsTestServer{sawCloseCh: make(chan bool, 1), wantHeader: wantHeader}
	w.srv = httptest.NewServer(wsHandler(t, mode, wantHeader, w.sawCloseCh))
	t.Cleanup(w.srv.Close)
	return w
}

func headerContains(h http.Header, name, want string) bool {
	for _, v := range h.Values(name) {
		if v == want {
			return true
		}
	}
	return false
}

// serveWS implements the server-side frame loop for one connection. Modes:
//
//	fragment  — echo every data frame back fragmented (2 fragments)
//	text      — echo every data frame back as text frames, fragmented
//	pong      — answer pings with pongs; echo data frames unfragmented
//	close-now — send a close frame immediately after the handshake
//	close-once— send a close frame after the first client data frame
//	observe   — read frames until close/EOF; report close seen on sawCloseCh
//	silent    — read frames without ever responding
func serveWS(t *testing.T, brw *bufio.ReadWriter, mode string, sawCloseCh chan bool) {
	t.Helper()
	br := brw.Reader
	bw := brw.Writer

	if mode == "close-now" {
		if err := sendTestFrame(bw, 0x8, []byte{0x03, 0xe8}); err != nil {
			t.Errorf("send close: %v", err)
		}
		return
	}

	for {
		op, payload, err := readTestFrame(br)
		if err != nil {
			return // client went away or closed cleanly
		}
		switch op {
		case 0x8:
			if mode == "observe" {
				select {
				case sawCloseCh <- true:
				default:
				}
			}
			return
		case 0x9: // ping
			if mode != "silent" {
				if err := sendTestFrame(bw, 0xA, payload); err != nil {
					t.Errorf("send pong: %v", err)
					return
				}
			}
		case 0x1, 0x2: // data
			switch mode {
			case "close-once":
				if err := sendTestFrame(bw, 0x8, []byte{0x03, 0xe8}); err != nil {
					t.Errorf("send close: %v", err)
				}
				return
			case "text":
				if err := echoFragmented(bw, 0x1, payload); err != nil {
					t.Errorf("echo text: %v", err)
					return
				}
			case "pong":
				if err := sendTestFrame(bw, 0x2, payload); err != nil {
					t.Errorf("echo: %v", err)
					return
				}
			case "fragment":
				if err := echoFragmented(bw, 0x2, payload); err != nil {
					t.Errorf("echo: %v", err)
					return
				}
			case "silent":
				// never respond
			}
		default:
			t.Errorf("server: unexpected opcode 0x%x", op)
			return
		}
	}
}

// readTestFrame parses one (possibly masked) client frame.
func readTestFrame(br *bufio.Reader) (op byte, payload []byte, err error) {
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return 0, nil, err
	}
	b0, b1 := h[0], h[1]
	op = b0 & 0x0f
	masked := b1&0x80 != 0
	var length uint64
	switch ln := b1 & 0x7f; {
	case ln < 126:
		length = uint64(ln)
	case ln == 126:
		var x [2]byte
		if _, err = io.ReadFull(br, x[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(x[:]))
	default:
		var x [8]byte
		if _, err = io.ReadFull(br, x[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(x[:])
	}
	var mk [4]byte
	if masked {
		if _, err = io.ReadFull(br, mk[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mk[i&3]
		}
	}
	return op, payload, nil
}

// sendTestFrame writes one unmasked (server-side) frame.
func sendTestFrame(bw *bufio.Writer, op byte, payload []byte) error {
	frame := make([]byte, 0, 10+len(payload))
	frame = append(frame, 0x80|op)
	switch {
	case len(payload) < 126:
		frame = append(frame, byte(len(payload)))
	case len(payload) <= 0xFFFF:
		frame = append(frame, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		var ln [8]byte
		binary.BigEndian.PutUint64(ln[:], uint64(len(payload)))
		frame = append(frame, 127)
		frame = append(frame, ln[:]...)
	}
	frame = append(frame, payload...)
	if _, err := bw.Write(frame); err != nil {
		return err
	}
	return bw.Flush()
}

// echoFragmented sends payload as two fragments: a non-final frame of the
// given data opcode plus one continuation frame.
func echoFragmented(bw *bufio.Writer, op byte, payload []byte) error {
	if len(payload) <= 1 {
		return sendTestFrame(bw, op, payload)
	}
	mid := len(payload) / 2
	first := make([]byte, 0, 10+mid)
	first = append(first, byte(op)) // FIN=0
	switch {
	case mid < 126:
		first = append(first, byte(mid))
	case mid <= 0xFFFF:
		first = append(first, 126, byte(mid>>8), byte(mid))
	default:
		var ln [8]byte
		binary.BigEndian.PutUint64(ln[:], uint64(mid))
		first = append(first, 127)
		first = append(first, ln[:]...)
	}
	first = append(first, payload[:mid]...)
	if _, err := bw.Write(first); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	return sendTestFrame(bw, 0x0, payload[mid:])
}

// -------- tests --------

// TestHandshakeAndEchoFragmented covers boundary payload lengths across the
// 7-bit/16-bit/64-bit frame length encodings, all served as fragmented echo.
func TestHandshakeAndEchoFragmented(t *testing.T) {
	w := newWSTestServer(t, "fragment", http.Header{"X-Test-Custom": []string{"yes"}})
	ctx := context.Background()
	c, err := Dial(ctx, w.srv.URL, http.Header{
		"X-Test-Custom": []string{"yes"},
		"Cookie":        []string{"sid=abc123"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	for _, size := range []int{0, 1, 2, 125, 126, 127, 65535, 65536, 70000, 200000} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte((i*7 + 3) % 251)
		}
		if err := c.WriteBinary(payload); err != nil {
			t.Fatalf("size %d: WriteBinary: %v", size, err)
		}
		got, err := c.ReadMessage(ctx)
		if err != nil {
			t.Fatalf("size %d: ReadMessage: %v", size, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("size %d: echo mismatch (%d bytes vs %d)", size, len(got), len(payload))
		}
	}
}

// TestTextFrameAccepted: server echoes using TEXT opcodes; text frames are
// accepted like binary ones.
func TestTextFrameAccepted(t *testing.T) {
	w := newWSTestServer(t, "text", nil)
	c, err := Dial(context.Background(), w.srv.URL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	msg := []byte("hello \u4e2d\u6587 text")
	if err := c.WriteBinary(msg); err != nil {
		t.Fatalf("WriteBinary: %v", err)
	}
	got, err := c.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q, want %q", got, msg)
	}
}

// TestPingPong: pings are answered with pongs; the pong traffic never leaks
// into ReadMessage results and data frames still flow.
func TestPingPong(t *testing.T) {
	w := newWSTestServer(t, "pong", nil)
	c, err := Dial(context.Background(), w.srv.URL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	for i := 0; i < 3; i++ {
		if err := c.WritePing(); err != nil {
			t.Fatalf("WritePing: %v", err)
		}
	}
	if err := c.WriteBinary([]byte("after pings")); err != nil {
		t.Fatalf("WriteBinary: %v", err)
	}
	got, err := c.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(got) != "after pings" {
		t.Fatalf("got %q", got)
	}
}

// TestServerCloseNowEOF: an immediate server close frame surfaces as io.EOF.
func TestServerCloseNowEOF(t *testing.T) {
	w := newWSTestServer(t, "close-now", nil)
	c, err := Dial(context.Background(), w.srv.URL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if _, err := c.ReadMessage(context.Background()); err != io.EOF {
		t.Fatalf("ReadMessage = %v, want io.EOF", err)
	}
}

// TestServerCloseOnceEOF: close after traffic surfaces as io.EOF.
func TestServerCloseOnceEOF(t *testing.T) {
	w := newWSTestServer(t, "close-once", nil)
	c, err := Dial(context.Background(), w.srv.URL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.WriteBinary([]byte("bye")); err != nil {
		t.Fatalf("WriteBinary: %v", err)
	}
	if _, err := c.ReadMessage(context.Background()); err != io.EOF {
		t.Fatalf("ReadMessage = %v, want io.EOF", err)
	}
}

// TestClientCloseSendsCloseFrame: the server observes a close frame before
// the TCP shutdown.
func TestClientCloseSendsCloseFrame(t *testing.T) {
	w := newWSTestServer(t, "observe", nil)
	c, err := Dial(context.Background(), w.srv.URL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.WriteBinary([]byte("x")); err != nil {
		t.Fatalf("WriteBinary: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case saw := <-w.sawCloseCh:
		if !saw {
			t.Fatal("server did not see a close frame")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server never reported the close frame")
	}
	// Second Close is a no-op; writes after Close fail with ErrClosed.
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := c.WriteBinary([]byte("late")); err != ErrClosed {
		t.Fatalf("WriteBinary after Close = %v, want ErrClosed", err)
	}
}

// TestReadMessageContextDeadline: a read blocked against a silent server
// honors a ctx deadline.
func TestReadMessageContextDeadline(t *testing.T) {
	w := newWSTestServer(t, "silent", nil)
	c, err := Dial(context.Background(), w.srv.URL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.WritePing(); err != nil {
		t.Fatalf("WritePing: %v", err)
	}
	start := time.Now()
	_, err = c.ReadMessage(ctx)
	if err == nil {
		t.Fatal("ReadMessage returned nil error on silent server")
	}
	if err == io.EOF {
		t.Fatalf("got io.EOF, want deadline error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("ReadMessage took too long")
	}
}

// TestWSSInsecureTLS: wss:// works against the self-signed httptest TLS
// server only when MEDIAMON_INSECURE_TLS=1 is set.
func TestWSSInsecureTLS(t *testing.T) {
	sawCloseCh := make(chan bool, 1)
	handler := wsHandler(t, "close-now", nil, sawCloseCh)
	tlssrv := httptest.NewTLSServer(handler)
	defer tlssrv.Close()

	t.Setenv("MEDIAMON_INSECURE_TLS", "1")
	c, err := Dial(context.Background(), tlssrv.URL, nil)
	if err != nil {
		t.Fatalf("Dial wss with insecure TLS: %v", err)
	}
	c.Close()

	t.Setenv("MEDIAMON_INSECURE_TLS", "")
	if _, err := Dial(context.Background(), tlssrv.URL, nil); err == nil {
		t.Fatal("Dial wss succeeded without MEDIAMON_INSECURE_TLS, want TLS verification failure")
	}
}

// TestDialErrors: invalid schemes, missing hosts and refused connections.
func TestDialErrors(t *testing.T) {
	if _, err := Dial(context.Background(), "ftp://host/x", nil); err == nil {
		t.Error("ftp scheme accepted")
	}
	if _, err := Dial(context.Background(), "ws:///nohost", nil); err == nil {
		t.Error("host-less url accepted")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadURL := "ws://" + l.Addr().String() + "/"
	_ = l.Close() // now nothing listens there
	if _, err := Dial(context.Background(), deadURL, nil); err == nil {
		t.Fatal("dial to closed port succeeded")
	}
}

// TestPropertyFrameRoundTrip: random payload lengths round-trip through a
// single connection, seeded via testkit.
func TestPropertyFrameRoundTrip(t *testing.T) {
	w := newWSTestServer(t, "fragment", nil)
	c, err := Dial(context.Background(), w.srv.URL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	prop := testkit.Prop{
		Name: "frame_payload_roundtrip",
		Inv: func(r *testkit.R) string {
			maxLen := 4096
			if r.Int63n(7) == 0 { // occasionally push past 16-bit length encoding
				maxLen = 70000
			}
			payload := r.Bytes(maxLen)
			if err := c.WriteBinary(payload); err != nil {
				return "write: " + err.Error()
			}
			got, err := c.ReadMessage(context.Background())
			if err != nil {
				return "read: " + err.Error()
			}
			if len(got) != len(payload) {
				return fmt.Sprintf("len %d != %d", len(got), len(payload))
			}
			for i := range payload {
				if got[i] != payload[i] {
					return fmt.Sprintf("byte %d differs", i)
				}
			}
			return ""
		},
	}
	testkit.Run(t, 20250825, 10, []testkit.Prop{prop})
	// A final exchange proves the connection is still healthy.
	if err := c.WriteBinary([]byte("last")); err != nil {
		t.Fatalf("WriteBinary: %v", err)
	}
	if got, err := c.ReadMessage(context.Background()); err != nil || string(got) != "last" {
		t.Fatalf("final echo = %q, %v", got, err)
	}
}
