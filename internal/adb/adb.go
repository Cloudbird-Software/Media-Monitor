// Package adb implements a minimal ADB transport client over TCP, hand-rolled
// per the P4 spec (stdlib only, no external adb tooling).
//
// Wire format (simplified binary variant of the ADB client/server protocol;
// note that a real `adb server` on 127.0.0.1:5037 uses 4 ASCII hex digits for
// its length prefixes instead of the 4-byte little-endian integers used
// here):
//
//	request:             [4-byte LE length][payload]
//	                     payload is the service string itself, e.g.
//	                     "host:transport:emulator-5554" or "shell:input tap".
//	reply, OKAY:         OKAY [4-byte LE len][segment]... [4-byte LE len=0]
//	                     a zero-length segment terminates the reply, so a
//	                     server may answer in multiple segments.
//	reply, FAIL:         FAIL [4-byte LE len][error message]
//	auth:                AUTH -> ErrAuthRequired. The CNXN handshake itself
//	                     is out of scope: the target device is expected to be
//	                     pre-authorized with the server.
//	shell/exec-out:      after OKAY the device streams
//	                     [4 ASCII hex digits][chunk]... terminated by the
//	                     segment "0000" or by the peer closing the connection.
//	                     Chunks may interleave stdout/stderr segments in any
//	                     arrival order; they are merged in arrival order.
//
// Device selection: every shell interaction first switches to the device with
// "host:transport:<serial>", then issues the shell service on the same
// connection. Serials of the form "host:port" (TCP-adbd devices) must already
// be registered with the server (`adb connect`) for transport to succeed.
package adb

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const dialTimeout = 10 * time.Second

// listDevicesTimeout bounds each host:devices exchange. Kept as a variable
// so tests can shorten it.
var listDevicesTimeout = 15 * time.Second

// ErrAuthRequired is returned when the server answers a request with AUTH:
// the target device is not authorized yet. The CNXN handshake is not
// implemented; register the device with the server first (adb connect).
var ErrAuthRequired = errors.New("adb: authentication required: register the device with the adb server first")

// Client is one TCP session to an adb server. All methods are safe for
// concurrent use; shell interactions are serialized per connection.
type Client struct {
	conn   net.Conn
	serial string // address this client was connected to (informational)
	mu     sync.Mutex
}

// Connect dials the adb server at addr (typically 127.0.0.1:5037) and
// returns a client ready for device-bound requests. The connection carries
// no per-device state: callers pass the serial on every method.
func Connect(addr string) (*Client, error) {
	if addr == "" {
		return nil, errors.New("adb: empty server address")
	}
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("adb: connect %s: %w", addr, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
	}
	return &Client{conn: conn, serial: addr}, nil
}

// Close terminates the server session.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// send runs one request/reply exchange for a host-level service (e.g.
// "host:transport:<serial>" or "host:devices") and returns the merged OKAY
// payload. FAIL replies surface as errors carrying the server message; AUTH
// replies surface as ErrAuthRequired.
func (c *Client) send(cmd string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendLocked(cmd)
}

// sendLocked is send with c.mu held.
func (c *Client) sendLocked(cmd string) ([]byte, error) {
	if c.conn == nil {
		return nil, errors.New("adb: client closed")
	}
	pkt := encodePacket(cmd)
	if _, err := c.conn.Write(pkt); err != nil {
		return nil, fmt.Errorf("adb: send %q: %w", cmd, err)
	}
	status, err := readStatus(c.conn)
	if err != nil {
		return nil, fmt.Errorf("adb: send %q: %w", cmd, err)
	}
	switch status {
	case "OKAY":
		var payload []byte
		for {
			seg, serr := readLEPayload(c.conn)
			if serr != nil {
				return nil, serr
			}
			if len(seg) == 0 {
				return payload, nil // zero-length segment terminates
			}
			payload = append(payload, seg...)
		}
	case "FAIL":
		msg, rerr := readLEPayload(c.conn)
		if rerr != nil {
			return nil, rerr
		}
		return nil, fmt.Errorf("adb: %q: %s", cmd, strings.TrimRight(string(msg), "\x00"))
	case "AUTH":
		return nil, ErrAuthRequired
	default:
		return nil, fmt.Errorf("adb: send %q: unexpected status %q", cmd, status)
	}
}

// encodePacket builds [4-byte LE length][payload].
func encodePacket(cmd string) []byte {
	pkt := make([]byte, 4+len(cmd))
	binary.LittleEndian.PutUint32(pkt[:4], uint32(len(cmd)))
	copy(pkt[4:], cmd)
	return pkt
}

// readStatus reads the 4-byte reply status word (OKAY/FAIL/AUTH/other).
func readStatus(conn net.Conn) (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		return "", fmt.Errorf("read status: %w", err)
	}
	return string(b[:]), nil
}

// readLEPayload reads one [4-byte LE length][payload] segment.
func readLEPayload(conn net.Conn) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, fmt.Errorf("adb: read reply length: %w", err)
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	payload := make([]byte, n)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, fmt.Errorf("adb: read reply payload (%d bytes): %w", n, err)
	}
	return payload, nil
}

// transportLocked switches the connection to device serial (host:transport).
func (c *Client) transportLocked(serial string) error {
	if _, err := c.sendLocked("host:transport:" + serial); err != nil {
		return fmt.Errorf("adb: transport %s: %w", serial, err)
	}
	return nil
}

// ExecOut runs shellCmd on the device through the exec-out service and
// returns its complete raw output. The stream is hex4-length framed; frames
// are merged in arrival order and the stream ends at a 0000 frame or when
// the peer closes the connection.
func (c *Client) ExecOut(serial, shellCmd string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.transportLocked(serial); err != nil {
		return nil, err
	}
	return c.streamLocked("shell:exec-out:"+shellCmd, true)
}

// Shell runs shellCmd on the device and returns its combined output as a
// string. Unlike ExecOut the shell: stream is raw (unframed) and ends when
// the connection closes.
func (c *Client) Shell(serial, shellCmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.transportLocked(serial); err != nil {
		return "", err
	}
	out, err := c.streamLocked("shell:"+shellCmd, false)
	return string(out), err
}

// streamLocked writes a shell service request and consumes its reply: OKAY
// starts the data stream (framed hex4 for exec-out, raw until close for
// shell), FAIL carries the error message, AUTH maps to ErrAuthRequired.
func (c *Client) streamLocked(service string, framed bool) ([]byte, error) {
	if c.conn == nil {
		return nil, errors.New("adb: client closed")
	}
	pkt := encodePacket(service)
	if _, err := c.conn.Write(pkt); err != nil {
		return nil, fmt.Errorf("adb: write %q: %w", service, err)
	}
	status, err := readStatus(c.conn)
	if err != nil {
		return nil, fmt.Errorf("adb: %q: %w", service, err)
	}
	switch status {
	case "OKAY":
		if framed {
			return c.readFrames()
		}
		data, err := io.ReadAll(c.conn)
		if err != nil {
			return nil, fmt.Errorf("adb: read %q output: %w", service, err)
		}
		return data, nil
	case "FAIL":
		msg, rerr := readLEPayload(c.conn)
		if rerr != nil {
			return nil, fmt.Errorf("adb: %q FAIL: %w", service, rerr)
		}
		return nil, fmt.Errorf("adb: %q: %s", service, strings.TrimRight(string(msg), "\x00"))
	case "AUTH":
		return nil, ErrAuthRequired
	default:
		return nil, fmt.Errorf("adb: %q: unexpected status %q", service, status)
	}
}

// readFrames drains a hex4-framed stream: [4 ASCII hex digits][chunk]...
// terminated by the segment "0000" or by EOF at a frame boundary (an
// exec-out device closes the connection instead of sending 0000). Chunks
// are merged in arrival order. A truncated header or data frame is a
// protocol error.
func (c *Client) readFrames() ([]byte, error) {
	var out []byte
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil // clean close; end of stream
			}
			return out, fmt.Errorf("adb: exec-out frame header: %w", err)
		}
		if string(hdr[:]) == "0000" {
			return out, nil
		}
		size, perr := strconv.ParseUint(string(hdr[:]), 16, 32)
		if perr != nil {
			return out, fmt.Errorf("adb: exec-out bad frame length %q", hdr)
		}
		chunk := make([]byte, int(size))
		if _, err := io.ReadFull(c.conn, chunk); err != nil {
			return out, fmt.Errorf("adb: exec-out frame (%d bytes): %w", size, err)
		}
		out = append(out, chunk...)
	}
}

// ListDevices queries the server at serverAddr (via its own fresh
// connection) with host:devices and returns the serials of every device in
// the "device" state; offline/unauthorized entries are skipped.
func (c *Client) ListDevices(serverAddr string) ([]string, error) {
	conn, err := net.DialTimeout("tcp", serverAddr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("adb: list-devices %s: %w", serverAddr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(listDevicesTimeout))
	tmp := &Client{conn: conn, serial: serverAddr}
	payload, err := tmp.send("host:devices")
	if err != nil {
		return nil, fmt.Errorf("adb: list-devices: %w", err)
	}
	return parseDeviceList(payload), nil
}

// parseDeviceList keeps the serials of lines "SERIAL\tstate" whose state is
// exactly "device".
func parseDeviceList(payload []byte) []string {
	var out []string
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		if strings.TrimSpace(fields[1]) != "device" {
			continue
		}
		out = append(out, strings.TrimSpace(fields[0]))
	}
	return out
}

// Tap sends an input tap at pixel (x, y).
func (c *Client) Tap(serial string, x, y int32) error {
	return c.shellInput(serial, fmt.Sprintf("input tap %d %d", x, y))
}

// Swipe sends an input swipe from (x0, y0) to (x1, y1) over ms milliseconds.
func (c *Client) Swipe(serial string, x0, y0, x1, y1 int32, ms int) error {
	return c.shellInput(serial, fmt.Sprintf("input swipe %d %d %d %d %d", x0, y0, x1, y1, ms))
}

// KeyText types text into the focused input. Spaces are encoded as %s and
// shell metacharacters are backslash-escaped, as `input text` expects.
func (c *Client) KeyText(serial, text string) error {
	return c.shellInput(serial, "input text "+escapeInputText(text))
}

// shellInput runs a raw shell command and discards its (usually empty)
// output; only transport/stream errors are reported.
func (c *Client) shellInput(serial, cmd string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.transportLocked(serial); err != nil {
		return err
	}
	_, err := c.streamLocked("shell:"+cmd, false)
	return err
}

// inputTextEscapes lists shell metacharacters `input text` can choke on;
// each is prefixed with a backslash. Spaces become %s (adb's encoding for
// the input text service).
const inputTextEscapes = "&|;<>()$`\"'\\*?[]{}#~!"

// escapeInputText prepares text for the `input text` service.
func escapeInputText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t':
			b.WriteString("%s")
		default:
			if strings.ContainsRune(inputTextEscapes, r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ScreencapPNG captures the screen via "screencap -p" and returns the raw
// PNG bytes.
func (c *Client) ScreencapPNG(serial string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.transportLocked(serial); err != nil {
		return nil, err
	}
	out, err := c.streamLocked("shell:exec-out:screencap -p", true)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UIDump returns the current UI hierarchy as a parsed NodeTree. Simplified
// implementation per P4: `uiautomator dump` writes its XML to
// /sdcard/window_dump.xml (and prints a "UI hierchary dumped to: <path>"
// hint line it would use to locate the file); we skip parsing that hint and
// exec-out `cat /sdcard/window_dump.xml` directly. uiDumpPath retains the
// hint-line parser for flows that run the dump command themselves.
func (c *Client) UIDump(serial string) (*NodeTree, error) {
	data, err := c.ExecOut(serial, "cat /sdcard/window_dump.xml")
	if err != nil {
		return nil, fmt.Errorf("adb: uidump: %w", err)
	}
	tree, err := ParseNodeTree(data)
	if err != nil {
		return nil, fmt.Errorf("adb: uidump: %w", err)
	}
	return tree, nil
}

// NodeTree is a parsed uiautomator XML hierarchy.
type NodeTree struct {
	Root *Node `xml:"node"`
}

// Node is one UI node of the hierarchy.
type Node struct {
	Text       string  `xml:"text,attr"`
	Class      string  `xml:"class,attr"`
	ResourceID string  `xml:"resource-id,attr"`
	Bounds     string  `xml:"bounds,attr"`
	Children   []*Node `xml:"node"`
}

// ParseNodeTree parses uiautomator dump XML (the body of
// /sdcard/window_dump.xml). A leading "UI hierchary dumped to: <path>" hint
// line, present in the output of a bare `uiautomator dump` run, is stripped
// before parsing.
func ParseNodeTree(data []byte) (*NodeTree, error) {
	if _, ok := uiDumpPath(data); ok {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	var tree NodeTree
	if err := xml.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("adb: parse ui XML: %w", err)
	}
	if tree.Root == nil {
		return nil, errors.New("adb: ui XML has no <node> element")
	}
	return &tree, nil
}

// uiDumpPath extracts the dump path from the leading hint line that
// `uiautomator dump` prints ("UI hierchary dumped to: /sdcard/..."). ok is
// false when the output starts with no such hint.
func uiDumpPath(out []byte) (string, bool) {
	nl := bytes.IndexByte(out, '\n')
	if nl < 0 {
		nl = len(out)
	}
	line := string(bytes.TrimRight(out[:nl], "\r"))
	const marker = "dumped to: "
	i := strings.LastIndex(line, marker)
	if i < 0 {
		return "", false
	}
	path := strings.TrimSpace(line[i+len(marker):])
	return path, path != ""
}

// Bounds is a parsed node bounds rectangle in screen pixels.
type Bounds struct {
	X1, Y1, X2, Y2 int
}

// ParseBounds parses the uiautomator bounds attribute of the form
// "[x1,y1][x2,y2]".
func ParseBounds(s string) (Bounds, error) {
	var b Bounds
	if _, err := fmt.Sscanf(s, "[%d,%d][%d,%d]", &b.X1, &b.Y1, &b.X2, &b.Y2); err != nil {
		return Bounds{}, fmt.Errorf("adb: parse bounds %q: %w", s, err)
	}
	if !strings.HasSuffix(strings.TrimSpace(s), "]") {
		return Bounds{}, fmt.Errorf("adb: parse bounds %q: trailing junk", s)
	}
	if b.X2 < b.X1 || b.Y2 < b.Y1 {
		return Bounds{}, fmt.Errorf("adb: inverted bounds %q", s)
	}
	return b, nil
}

// Center returns the pixel coordinates of the rectangle's center.
func (b Bounds) Center() (int32, int32) {
	return int32((b.X1 + b.X2) / 2), int32((b.Y1 + b.Y2) / 2)
}

// ClickCenter returns the tap coordinates at the center of node's bounds.
func ClickCenter(n *Node) (int32, int32, error) {
	if n == nil {
		return 0, 0, errors.New("adb: nil node")
	}
	b, err := ParseBounds(n.Bounds)
	if err != nil {
		return 0, 0, err
	}
	x, y := b.Center()
	return x, y, nil
}
