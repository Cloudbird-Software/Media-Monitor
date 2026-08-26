// Package wsutil implements a minimal RFC 6455 WebSocket client (stdlib
// only). Frame parsing, masking and fragmentation reassembly are hand-rolled;
// the UTF-8 validation of text payloads is intentionally omitted (payloads
// are treated as opaque bytes). TLS for wss:// uses the trust settings of a
// default TLS client; certificate verification can be skipped for local
// testing by setting the environment variable MEDIAMON_INSECURE_TLS=1
// (test-only escape hatch, never for production).
package wsutil

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrClosed is returned when operating on an already-closed connection.
var ErrClosed = errors.New("wsutil: connection closed")

const wsAcceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Conn is a client WebSocket connection. Writes are serialized internally;
// use ReadMessage from a single goroutine at a time.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu    sync.Mutex // serializes outgoing frames and Close
	sockClosed bool       // set under writeMu; guards the socket after Close
}

// Dial opens a WebSocket connection to urlStr (ws:// and wss://; http/https
// are accepted as aliases). headers are sent during the handshake. ctx may
// carry a deadline that bounds dialing and the handshake.
func Dial(ctx context.Context, urlStr string, headers http.Header) (*Conn, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("wsutil: parse url: %w", err)
	}
	var tlsOn bool
	switch u.Scheme {
	case "ws", "http":
	case "wss", "https":
		tlsOn = true
	default:
		return nil, fmt.Errorf("wsutil: unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("wsutil: url has no host")
	}

	host := u.Hostname()
	addr := u.Host
	if u.Port() == "" {
		if tlsOn {
			addr = net.JoinHostPort(host, "443")
		} else {
			addr = net.JoinHostPort(host, "80")
		}
	}

	dialer := &net.Dialer{}
	if dl, ok := ctx.Deadline(); ok {
		dialer.Deadline = dl
	}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("wsutil: dial %s: %w", addr, err)
	}

	if tlsOn {
		cfg := &tls.Config{ServerName: host}
		if os.Getenv("MEDIAMON_INSECURE_TLS") == "1" {
			// Test-only: accept self-signed certificates against local
			// test servers. Never set this in production.
			cfg.InsecureSkipVerify = true
		}
		ts := tls.Client(raw, cfg)
		if err := ts.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("wsutil: tls handshake: %w", err)
		}
		raw = ts
	}

	// Bound the handshake so a misbehaving server cannot hold the socket.
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	} else {
		_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	}

	c := &Conn{conn: raw, br: bufio.NewReader(raw)}
	if err := c.handshake(u, headers); err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return c, nil
}

// handshake writes the RFC 6455 opening handshake and verifies the 101
// response, including the Sec-WebSocket-Accept key.
func (c *Conn) handshake(u *url.URL, headers http.Header) error {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return fmt.Errorf("wsutil: key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", u.Host)
	fmt.Fprintf(&req, "Upgrade: websocket\r\n")
	fmt.Fprintf(&req, "Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	fmt.Fprintf(&req, "Sec-WebSocket-Version: 13\r\n")
	for k, vs := range headers {
		for _, v := range vs {
			fmt.Fprintf(&req, "%s: %s\r\n", k, v)
		}
	}
	req.WriteString("\r\n")
	if _, err := c.conn.Write([]byte(req.String())); err != nil {
		return fmt.Errorf("wsutil: handshake write: %w", err)
	}

	resp, err := http.ReadResponse(c.br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return fmt.Errorf("wsutil: handshake read: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("wsutil: handshake rejected with status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != acceptKey(key) {
		return fmt.Errorf("wsutil: bad Sec-WebSocket-Accept (got %q)", got)
	}
	return nil
}

// acceptKey computes the server-side Sec-WebSocket-Accept value.
func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsAcceptGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// readFrame reads one WebSocket frame: FIN bit, opcode, and the unmasked
// payload (masking from either direction is handled, although RFC 6455 only
// requires it client->server). Returns io.EOF on a clean TCP shutdown.
func (c *Conn) readFrame() (opcode byte, fin bool, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return 0, false, nil, err
	}
	b0, b1 := h[0], h[1]
	if b0&0x70 != 0 {
		return 0, false, nil, errors.New("wsutil: reserved bits set")
	}
	fin = b0&0x80 != 0
	opcode = b0 & 0x0f
	masked := b1&0x80 != 0

	var length uint64
	switch ln := b1 & 0x7f; {
	case ln < 126:
		length = uint64(ln)
	case ln == 126:
		var x [2]byte
		if _, err = io.ReadFull(c.br, x[:]); err != nil {
			return 0, false, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(x[:]))
	default:
		var x [8]byte
		if _, err = io.ReadFull(c.br, x[:]); err != nil {
			return 0, false, nil, err
		}
		length = binary.BigEndian.Uint64(x[:])
	}

	// Control frames are <= 125 bytes and must not be fragmented.
	if opcode >= 0x8 {
		if length > 125 {
			return 0, false, nil, errors.New("wsutil: control frame too long")
		}
		if !fin {
			return 0, false, nil, errors.New("wsutil: fragmented control frame")
		}
	}

	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, maskKey[:]); err != nil {
			return 0, false, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return 0, false, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i&3]
		}
	}
	return opcode, fin, payload, nil
}

// ReadMessage returns the next complete message: text (0x1) and binary (0x2)
// frames are both accepted and reassembled across fragments, pings are
// answered with pongs transparently, pongs are ignored, and a close frame
// (or a dead socket) terminates with io.EOF. ctx may carry a deadline;
// cancellation without a deadline is observed at the next read boundary.
func (c *Conn) ReadMessage(ctx context.Context) ([]byte, error) {
	if err := c.prepareReadDeadline(ctx); err != nil {
		return nil, err
	}
	defer c.clearReadDeadline()

	var buf []byte
	var open bool // a fragmented message is in progress
	for {
		op, fin, payload, err := c.readFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, io.EOF
			}
			return nil, err
		}
		switch op {
		case 0x9: // ping -> pong with identical payload
			if werr := c.writeFrame(0xA, payload); werr != nil {
				return nil, werr
			}
		case 0xA: // pong: nothing to do
		case 0x8: // close: echo and terminate
			_ = c.writeFrame(0x8, payload)
			return nil, io.EOF
		case 0x1, 0x2: // text/binary data frame
			if open {
				return nil, errors.New("wsutil: data frame inside a fragmented message")
			}
			open = !fin
			buf = append(buf[:0], payload...)
			if fin {
				return buf, nil
			}
		case 0x0: // continuation
			if !open {
				return nil, errors.New("wsutil: unexpected continuation frame")
			}
			buf = append(buf, payload...)
			if fin {
				open = false
				return buf, nil
			}
		default:
			return nil, fmt.Errorf("wsutil: unsupported opcode 0x%x", op)
		}
	}
}

// prepareReadDeadline maps a ctx deadline onto the socket; returns ctx.Err()
// if the context is already done.
func (c *Conn) prepareReadDeadline(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		return c.conn.SetReadDeadline(dl)
	}
	return nil
}

func (c *Conn) clearReadDeadline() { _ = c.conn.SetReadDeadline(time.Time{}) }

// WriteBinary sends payload as one masked binary message (client frames are
// always masked per RFC 6455 §5.1).
func (c *Conn) WriteBinary(payload []byte) error {
	return c.writeFrame(0x2, payload)
}

// WritePing sends a masked ping with an empty payload.
func (c *Conn) WritePing() error {
	return c.writeFrame(0x9, nil)
}

// writeFrame serializes one masked client frame and writes it to the socket.
func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.sockClosed {
		return ErrClosed
	}
	return c.writeFrameLocked(opcode, payload)
}

// writeFrameLocked writes a frame; callers must hold writeMu (except Close,
// which sets sockClosed first).
func (c *Conn) writeFrameLocked(opcode byte, payload []byte) error {
	var maskKey [4]byte
	if _, err := rand.Read(maskKey[:]); err != nil {
		return fmt.Errorf("wsutil: mask key: %w", err)
	}

	// Encode the frame header into a small, fixed-capacity buffer. The header is
	// at most 10 bytes (2 + 8 length), so its allocation is bounded and cannot
	// overflow. The masked payload is appended separately — combining the two
	// sizes in a single make() would risk an int overflow for huge payloads
	// (CodeQL go/incorrect-integer-conversion / allocation-overflow).
	hdr := make([]byte, 0, 10)
	hdr = append(hdr, 0x80|opcode) // FIN + opcode
	switch {
	case len(payload) < 126:
		hdr = append(hdr, 0x80|byte(len(payload))) // MASK + length
	case len(payload) <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	default:
		var ln [8]byte
		binary.BigEndian.PutUint64(ln[:], uint64(len(payload)))
		hdr = append(hdr, 0x80|127)
		hdr = append(hdr, ln[:]...)
	}
	hdr = append(hdr, maskKey[:]...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ maskKey[i&3]
	}
	frame := make([]byte, 0, len(hdr))
	frame = append(frame, hdr...)
	frame = append(frame, masked...)
	_, err := c.conn.Write(frame)
	return err
}

// Close sends a best-effort close frame (code 1000) and closes the socket.
// Close is idempotent; subsequent writes fail with ErrClosed.
func (c *Conn) Close() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.sockClosed {
		return nil
	}
	c.sockClosed = true
	// Best-effort close handshake: ignore write errors here.
	_ = c.writeFrameLocked(0x8, []byte{0x03, 0xe8})
	return c.conn.Close()
}
