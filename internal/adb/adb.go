// Package adb implements a minimal ADB client/server transport over TCP,
// stdlib-only. Wire format follows the real adb server on 127.0.0.1:5037:
//
//   - requests are framed as [4 ASCII hex length digits][payload];
//   - a reply starts with OKAY or FAIL; FAIL is followed by
//     [4 ASCII hex length digits][message];
//   - after OKAY, data depends on the service:
//   - host:transport returns a bare OKAY (the connection is then bound to
//     the device for subsequent services on the same socket);
//   - shell:<cmd> returns OKAY then a raw byte stream until the peer closes
//     the connection;
//   - exec:<cmd> returns OKAY then hex4-framed chunks (stdout/stderr may
//     interleave) terminated by the "0000" end marker or by EOF;
//   - host:devices returns OKAY then hex4-framed lines terminated by "0000".
//
// The CNXN handshake itself is out of scope: the target device must already
// be registered/authorized with the adb server.
package adb

import (
	"bytes"
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
// the target device is not authorized yet.
var ErrAuthRequired = errors.New("adb: authentication required: register the device with the adb server first")

// Client is one TCP session to an adb server. Methods are safe for
// concurrent use; per-client device interactions are serialized.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	dev  string // currently bound device serial ("" = none yet)
}

// Connect opens a TCP connection to the adb server (e.g. 127.0.0.1:5037).
func Connect(serverAddr string) (*Client, error) {
	conn, err := net.DialTimeout("tcp", serverAddr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("adb: connect %s: %w", serverAddr, err)
	}
	return &Client{conn: conn}, nil
}

// Close terminates the session.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// ListDevices queries the server with host:devices over a fresh connection
// and returns the serials of every device in the "device" state; offline and
// unauthorized entries are skipped.
func (c *Client) ListDevices(serverAddr string) ([]string, error) {
	conn, err := net.DialTimeout("tcp", serverAddr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("adb: list-devices %s: %w", serverAddr, err)
	}
	defer conn.Close()
	tmp := &Client{conn: conn}
	_ = conn.SetDeadline(time.Now().Add(listDevicesTimeout))
	if err := tmp.send("host:devices"); err != nil {
		return nil, fmt.Errorf("adb: list-devices: %w", err)
	}
	var status [4]byte
	if _, err := io.ReadFull(conn, status[:]); err != nil {
		return nil, fmt.Errorf("adb: list-devices reply: %w", err)
	}
	if string(status[:]) == "FAIL" {
		msg, err := tmp.readPayload()
		if err != nil {
			return nil, fmt.Errorf("adb: list-devices: %w", err)
		}
		return nil, fmt.Errorf("adb: list-devices: %s", strings.TrimSpace(string(msg)))
	}
	if string(status[:]) != "OKAY" {
		return nil, fmt.Errorf("adb: unexpected list-devices reply %q", status)
	}
	chunks, err := tmp.readHexFrames()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("adb: list-devices: %w", err)
	}
	var serials []string
	for _, line := range strings.Split(string(bytes.Join(chunks, nil)), "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == "device" {
			serials = append(serials, parts[0])
		}
	}
	return serials, nil
}

// bind switches the connection to the given device serial.
func (c *Client) bind(serial string) error {
	if c.dev == serial {
		return nil
	}
	if err := c.send("host:transport:" + serial); err != nil {
		return err
	}
	var status [4]byte
	if _, err := io.ReadFull(c.conn, status[:]); err != nil {
		return fmt.Errorf("adb: transport reply: %w", err)
	}
	switch string(status[:]) {
	case "OKAY":
		c.dev = serial
		return nil
	case "AUTH":
		return ErrAuthRequired
	case "FAIL":
		msg, err := c.readPayload()
		if err != nil {
			return fmt.Errorf("adb: transport refused (%w)", err)
		}
		return fmt.Errorf("adb: transport refused: %s", strings.TrimSpace(string(msg)))
	default:
		return fmt.Errorf("adb: unexpected transport reply %q", status)
	}
}

// Shell runs a shell command on the device and returns the raw combined
// output. The shell service streams data until the peer closes.
func (c *Client) Shell(serial, shellCmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.bind(serial); err != nil {
		return "", err
	}
	if err := c.send("shell:" + shellCmd); err != nil {
		return "", err
	}
	var status [4]byte
	if _, err := io.ReadFull(c.conn, status[:]); err != nil {
		return "", fmt.Errorf("adb: shell reply: %w", err)
	}
	if string(status[:]) == "FAIL" {
		msg, err := c.readPayload()
		if err != nil {
			return "", fmt.Errorf("adb: shell error: %w", err)
		}
		return "", fmt.Errorf("adb: shell error: %s", strings.TrimSpace(string(msg)))
	}
	if string(status[:]) != "OKAY" {
		return "", fmt.Errorf("adb: unexpected shell reply %q", status)
	}
	// Raw stream until close — shell output has no explicit framing.
	out, err := io.ReadAll(c.conn)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("adb: shell output: %w", err)
	}
	return string(out), nil
}

// ExecOut runs a command via the exec service (hex4-framed output,
// stdout/stderr merged in arrival order) and returns the combined bytes.
func (c *Client) ExecOut(serial, shellCmd string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.bind(serial); err != nil {
		return nil, err
	}
	if err := c.send("exec:" + shellCmd); err != nil {
		return nil, err
	}
	status := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, status); err != nil {
		return nil, fmt.Errorf("adb: exec reply: %w", err)
	}
	if string(status) == "FAIL" {
		msg, err := c.readPayload()
		if err != nil {
			return nil, fmt.Errorf("adb: exec error: %w", err)
		}
		return nil, fmt.Errorf("adb: exec error: %s", strings.TrimSpace(string(msg)))
	}
	if string(status) != "OKAY" {
		return nil, fmt.Errorf("adb: unexpected exec reply %q", status)
	}
	chunks, err := c.readHexFrames()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("adb: exec-out: %w", err)
	}
	return bytes.Join(chunks, nil), nil
}

// Tap sends `input tap x y`.
func (c *Client) Tap(serial string, x, y int32) error {
	_, err := c.ExecOut(serial, fmt.Sprintf("input tap %d %d", x, y))
	return err
}

// Swipe sends `input swipe ...`.
func (c *Client) Swipe(serial string, x0, y0, x1, y1 int32, ms int) error {
	_, err := c.ExecOut(serial, fmt.Sprintf("input swipe %d %d %d %d %d", x0, y0, x1, y1, ms))
	return err
}

// KeyText sends `input text <escaped>`; spaces become %s.
func (c *Client) KeyText(serial, text string) error {
	escaped := strings.ReplaceAll(text, " ", "%s")
	_, err := c.ExecOut(serial, "input text "+escaped)
	return err
}

// ScreencapPNG returns the raw PNG screenshot bytes.
func (c *Client) ScreencapPNG(serial string) ([]byte, error) {
	return c.ExecOut(serial, "screencap -p")
}

// NodeTree is the parsed uiautomator hierarchy.
type NodeTree struct {
	Root *Node
}

// Node is one UI element with attributes and children.
type Node struct {
	Text       string
	Class      string
	ResourceID string
	Bounds     string
	Clickable  bool
	Children   []*Node
}

// BoundsRect is the parsed "[x1,y1][x2,y2]" bounds attribute.
type BoundsRect struct {
	X1, Y1, X2, Y2 int
}

// ParseBounds parses a bounds attribute.
func ParseBounds(s string) (BoundsRect, error) {
	var r BoundsRect
	_, err := fmt.Sscanf(s, "[%d,%d][%d,%d]", &r.X1, &r.Y1, &r.X2, &r.Y2)
	return r, err
}

// Center returns the center point of the bounds.
func (r BoundsRect) Center() (int, int) {
	return (r.X1 + r.X2) / 2, (r.Y1 + r.Y2) / 2
}

// UIDump refreshes the UI hierarchy (uiautomator dump) and returns it.
func (c *Client) UIDump(serial string) (*NodeTree, error) {
	out, err := c.ExecOut(serial, "uiautomator dump /sdcard/window_dump.xml")
	if err != nil {
		return nil, err
	}
	path := uiDumpPath(string(out))
	if path == "" {
		path = "/sdcard/window_dump.xml"
	}
	data, err := c.ExecOut(serial, "cat "+path)
	if err != nil {
		return nil, fmt.Errorf("adb: uidump cat %s: %w", path, err)
	}
	return ParseNodeTree(data)
}

// uiDumpPath extracts the dumped file path from the "UI hierchary dumped
// to: /path" report line.
func uiDumpPath(out string) string {
	const marker = "dumped to: "
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			return strings.TrimSpace(line[i+len(marker):])
		}
	}
	return ""
}

// ParseNodeTree parses a uiautomator XML document, stripping a leading
// dump-report line when present.
func ParseNodeTree(data []byte) (*NodeTree, error) {
	doc := string(data)
	if i := strings.Index(doc, "<?xml"); i > 0 {
		doc = doc[i:]
	}
	type xmlNode struct {
		Text       string    `xml:"text,attr"`
		Class      string    `xml:"class,attr"`
		ResourceID string    `xml:"resource-id,attr"`
		Bounds     string    `xml:"bounds,attr"`
		Clickable  string    `xml:"clickable,attr"`
		Nodes      []xmlNode `xml:"node"`
	}
	var root xmlNode
	if err := xml.Unmarshal([]byte(doc), &root); err != nil {
		return nil, fmt.Errorf("adb: uidump xml: %w", err)
	}
	var convert func(n xmlNode) *Node
	convert = func(n xmlNode) *Node {
		out := &Node{
			Text:       n.Text,
			Class:      n.Class,
			ResourceID: n.ResourceID,
			Bounds:     n.Bounds,
			Clickable:  strings.EqualFold(n.Clickable, "true"),
		}
		for _, ch := range n.Nodes {
			out.Children = append(out.Children, convert(ch))
		}
		return out
	}
	rootNode := convert(root)
	for rootNode.Text == "" && rootNode.Class == "" && len(rootNode.Children) == 1 {
		rootNode = rootNode.Children[0]
	}
	return &NodeTree{Root: rootNode}, nil
}

// send writes one hex4-framed request payload.
func (c *Client) send(payload string) error {
	if len(payload) > 0xFFFF {
		return fmt.Errorf("adb: request too large (%d bytes)", len(payload))
	}
	if _, err := fmt.Fprintf(c.conn, "%04x%s", len(payload), payload); err != nil {
		return fmt.Errorf("adb: write request: %w", err)
	}
	return nil
}

// readPayload reads one hex4-length-framed payload (used after FAIL).
func (c *Client) readPayload() ([]byte, error) {
	n, err := readHexLen(c.conn)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// readHexFrames drains a hex4-framed reply stream until the "0000" marker
// or EOF, merging chunks in arrival order. The leading OKAY/FAIL status must
// already be consumed by the caller.
func (c *Client) readHexFrames() ([][]byte, error) {
	var out [][]byte
	for {
		n, err := readHexLen(c.conn)
		if err != nil {
			return out, err // io.EOF tolerated by callers
		}
		if n == 0 {
			return out, nil
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.conn, buf); err != nil {
			return out, err
		}
		out = append(out, buf)
	}
}

// readHexLen reads the 4 ASCII hex digits framing the next chunk.
func readHexLen(r io.Reader) (int, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, err
	}
	raw := string(hdr[:])
	if raw == "OKAY" {
		return 0, fmt.Errorf("adb: unexpected OKAY inside framed stream")
	}
	n, err := strconv.ParseUint(raw, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("adb: bad frame length %q", raw)
	}
	return int(n), nil
}

// hexFrames is a test helper building a hex4 stream terminated with "0000".
func hexFrames(parts ...string) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		fmt.Fprintf(&b, "%04x", len(p))
		b.WriteString(p)
	}
	b.WriteString("0000")
	return b.Bytes()
}
