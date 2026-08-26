package adb

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeServer implements a minimal adb-server wire twin for tests: hex4
// request framing, OKAY/FAIL/AUTH replies, hex4-framed exec streams and raw
// shell streams. It records every received request for injection-order
// assertions.
type fakeServer struct {
	t   *testing.T
	mu  sync.Mutex
	got []string
	// handler answers each request; called inside the connection goroutine
	// via t.Errorf for assertion failures (per-conn handler grossness).
	handler func(req string, w *bufio.ReadWriter) error
}

func (f *fakeServer) record(req string) {
	f.mu.Lock()
	f.got = append(f.got, req)
	f.mu.Unlock()
}

func (f *fakeServer) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// startFake launches the server and returns a connected client.
func startFake(t *testing.T, handler func(req string, w *bufio.ReadWriter) error) (*Client, *fakeServer) {
	t.Helper()
	f := &fakeServer{t: t, handler: handler}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				bw := bufio.NewWriter(conn)
				for {
					var hdr [4]byte
					if _, err := io.ReadFull(br, hdr[:]); err != nil {
						return
					}
					n, err := parseHexLen(string(hdr[:]))
					if err != nil || n == 0 {
						return
					}
					req := make([]byte, n)
					if _, err := io.ReadFull(br, req); err != nil {
						return
					}
					f.record(string(req))
					if f.handler != nil {
						err := f.handler(string(req), bufio.NewReadWriter(br, bw))
						_ = bw.Flush()
						if err != nil {
							return
						}
					}
				}
			}(conn)
		}
	}()
	c, err := Connect(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, f
}

var errCloseConn = fmt.Errorf("close fake session")

func parseHexLen(h string) (int, error) {
	if h == "OKAY" {
		return 0, fmt.Errorf("OKAY")
	}
	var n int
	_, err := fmt.Sscanf(h, "%04x", &n)
	return n, err
}

func writeOKAY(w io.Writer) { _, _ = w.Write([]byte("OKAY")) }

func writeFrames(w io.Writer, parts ...string) {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	_, _ = w.Write(hexFrames(kept...))
}

// execHandler answers transport+exec requests with chunked output.
func execHandler(w *bufio.ReadWriter, req, out string) error {
	if strings.HasPrefix(req, "host:transport:") {
		writeOKAY(w)
		return nil
	}
	if strings.HasPrefix(req, "exec:") {
		writeOKAY(w)
		writeFrames(w, out)
		return nil
	}
	return fmt.Errorf("unexpected request %q", req)
}

// TestListDevices verifies the wire request (host:devices) and the parsing
// of the device-list response: only serials in the "device" state are
// returned, offline/unauthorized ones are filtered.
func TestListDevices(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		var hdr [4]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return
		}
		n, _ := parseHexLen(string(hdr[:]))
		req := make([]byte, n)
		if _, err := io.ReadFull(br, req); err != nil {
			return
		}
		if string(req) != "host:devices" {
			t.Errorf("request = %q, want host:devices", req)
			return
		}
		_, _ = conn.Write([]byte("OKAY"))
		_, _ = conn.Write(hexFrames("emulator-5554\tdevice\n123abc\toffline\n"))
	}()
	c, _ := startFake(t, nil)
	serials, err := c.ListDevices(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if len(serials) != 1 || serials[0] != "emulator-5554" {
		t.Fatalf("serials = %v, want [emulator-5554] (only online devices)", serials)
	}
}

func TestListDevicesOwnConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		var hdr [4]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return
		}
		n, _ := parseHexLen(string(hdr[:]))
		req := make([]byte, n)
		if _, err := io.ReadFull(br, req); err != nil {
			return
		}
		if string(req) != "host:devices" {
			t.Errorf("request = %q", req)
			return
		}
		_, _ = conn.Write([]byte("OKAY"))
		_, _ = conn.Write(hexFrames("ser-1\tdevice\nser-2\tunauthorized\n"))
	}()
	c, _ := startFake(t, nil)
	serials, err := c.ListDevices(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if len(serials) != 1 || serials[0] != "ser-1" {
		t.Fatalf("serials = %v", serials)
	}
}

func TestExecOutFramesMerged(t *testing.T) {
	c, fs := startFake(t, func(req string, w *bufio.ReadWriter) error {
		switch {
		case strings.HasPrefix(req, "host:transport:ser-1"):
			writeOKAY(w)
		case req == "exec:echo hi":
			writeOKAY(w)
			writeFrames(w, "hello ", "world")
		case req == "exec:echo x":
			writeOKAY(w)
			writeFrames(w, "x")
		default:
			return fmt.Errorf("unexpected %q", req)
		}
		return nil
	})
	out, err := c.ExecOut("ser-1", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello world" {
		t.Fatalf("out = %q", out)
	}
	reqs := fs.requests()
	if len(reqs) != 2 || !strings.HasPrefix(reqs[0], "host:transport:ser-1") || reqs[1] != "exec:echo hi" {
		t.Fatalf("requests = %v", reqs)
	}
	// Second exec on the same bound device must not re-issue the transport.
	if _, err := c.ExecOut("ser-1", "echo x"); err != nil {
		t.Fatal(err)
	}
	reqs = fs.requests()
	if strings.Join(reqs, "|") != "host:transport:ser-1|exec:echo hi|exec:echo x" {
		t.Fatalf("requests after rebind = %v", reqs)
	}
}

func TestShellRawStream(t *testing.T) {
	c, _ := startFake(t, func(req string, w *bufio.ReadWriter) error {
		if strings.HasPrefix(req, "host:transport:") {
			writeOKAY(w)
			return nil
		}
		writeOKAY(w)
		_, _ = w.WriteString("raw-output")
		return errCloseConn // shell sessions close after the stream
	})
	out, err := c.Shell("ser-1", "dumpsys")
	if err != nil {
		t.Fatal(err)
	}
	if out != "raw-output" {
		t.Fatalf("shell out = %q", out)
	}
}

func TestTransportFailure(t *testing.T) {
	c, _ := startFake(t, func(req string, w *bufio.ReadWriter) error {
		_, _ = w.WriteString("FAIL")
		fmt.Fprintf(w, "%04xno such device", len("no such device"))
		return errCloseConn
	})
	if _, err := c.ExecOut("bad", "ls"); err == nil || !strings.Contains(err.Error(), "no such device") {
		t.Fatalf("err = %v", err)
	}
}

func TestTapSwipeKeyTextInjection(t *testing.T) {
	c, fs := startFake(t, func(req string, w *bufio.ReadWriter) error {
		return execHandler(w, req, "")
	})
	if err := c.Tap("s", 10, 20); err != nil {
		t.Fatal(err)
	}
	if err := c.Swipe("s", 0, 0, 100, 200, 300); err != nil {
		t.Fatal(err)
	}
	if err := c.KeyText("s", "hello world"); err != nil {
		t.Fatal(err)
	}
	reqs := fs.requests()
	want := []string{
		"host:transport:s", "exec:input tap 10 20",
		"exec:input swipe 0 0 100 200 300",
		"exec:input text hello%sworld",
	}
	if strings.Join(reqs, "|") != strings.Join(want, "|") {
		t.Fatalf("requests = %v", reqs)
	}
}

func TestScreencapPNG(t *testing.T) {
	c, _ := startFake(t, func(req string, w *bufio.ReadWriter) error {
		return execHandler(w, req, "\x89PNG-fake")
	})
	png, err := c.ScreencapPNG("s")
	if err != nil {
		t.Fatal(err)
	}
	if string(png) != "\x89PNG-fake" {
		t.Fatalf("png = %q", png)
	}
}

const uiXML = `<?xml version="1.0" encoding="UTF-8"?>
<hierarchy rotation="0">
  <node text="" class="android.widget.FrameLayout" resource-id="" bounds="[0,0][1080,2400]" clickable="false">
    <node text="评论" class="android.widget.TextView" resource-id="comment-btn" bounds="[50,60][350,140]" clickable="true"/>
  </node>
</hierarchy>`

func TestUIDump(t *testing.T) {
	c, fs := startFake(t, func(req string, w *bufio.ReadWriter) error {
		switch {
		case strings.HasPrefix(req, "host:transport:"):
			writeOKAY(w)
		case strings.HasPrefix(req, "exec:uiautomator"):
			writeOKAY(w)
			writeFrames(w, "UI hierchary dumped to: /sdcard/window_dump.xml")
		case strings.HasPrefix(req, "exec:cat"):
			writeOKAY(w)
			writeFrames(w, uiXML)
		default:
			return fmt.Errorf("unexpected %q", req)
		}
		return nil
	})
	tree, err := c.UIDump("s")
	if err != nil {
		t.Fatal(err)
	}
	if tree.Root == nil || len(tree.Root.Children) != 1 {
		t.Fatalf("tree = %+v", tree)
	}
	btn := tree.Root.Children[0]
	if btn.Text != "评论" || btn.ResourceID != "comment-btn" || !btn.Clickable {
		t.Fatalf("node = %+v", btn)
	}
	rect, err := ParseBounds(btn.Bounds)
	if err != nil {
		t.Fatal(err)
	}
	x, y := rect.Center()
	if x != 200 || y != 100 {
		t.Fatalf("center = %d,%d", x, y)
	}
	reqs := fs.requests()
	if len(reqs) < 3 || !strings.HasPrefix(reqs[2], "exec:cat /sdcard/window_dump.xml") {
		t.Fatalf("uidump requests = %v", reqs)
	}
}

func TestParseNodeTreeStripsReportLine(t *testing.T) {
	data := []byte("UI hierchary dumped to: /sdcard/window_dump.xml\n" + uiXML)
	tree, err := ParseNodeTree(data)
	if err != nil || tree.Root == nil {
		t.Fatalf("tree=%+v err=%v", tree, err)
	}
}

func TestAuthRequired(t *testing.T) {
	c, _ := startFake(t, func(req string, w *bufio.ReadWriter) error {
		_, _ = w.WriteString("AUTH")
		return nil
	})
	if err := c.Tap("s", 1, 2); err != ErrAuthRequired {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
}

func TestHexLenErrors(t *testing.T) {
	if _, err := readHexLen(strings.NewReader("ZZZZ")); err == nil {
		t.Fatal("bad frame length accepted")
	}
	if n, err := readHexLen(strings.NewReader("00ff")); err != nil || n != 255 {
		t.Fatalf("00ff = %d, %v", n, err)
	}
}
