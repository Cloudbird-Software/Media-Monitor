package adb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fake ADB server
//
// Reads the client's 4-byte little-endian length + payload packets, records
// every received command (raw bytes and service string, in arrival order)
// and answers with the preset whose prefix greedily matches the service.
// ---------------------------------------------------------------------------

// enc builds the wire packet [4-byte LE length][cmd].
func enc(cmd string) []byte {
	pkt := make([]byte, 4+len(cmd))
	binary.LittleEndian.PutUint32(pkt, uint32(len(cmd)))
	copy(pkt[4:], cmd)
	return pkt
}

// okReply builds an OKAY reply carrying zero or more segments, terminated by
// the zero-length sentinel (multi-segment capable).
func okReply(segs ...[]byte) []byte {
	b := []byte("OKAY")
	hdr := make([]byte, 4)
	for _, s := range segs {
		binary.LittleEndian.PutUint32(hdr, uint32(len(s)))
		b = append(b, hdr...)
		b = append(b, s...)
	}
	binary.LittleEndian.PutUint32(hdr, 0)
	return append(b, hdr...)
}

// failReply builds a FAIL reply with one error-message payload.
func failReply(msg string) []byte {
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr, uint32(len(msg)))
	return append(append([]byte("FAIL"), hdr...), msg...)
}

// streamReply builds a hex4-framed stream body: [4 hex digits][chunk]... and
// the terminating 0000 frame.
func streamReply(chunks ...[]byte) []byte {
	var b []byte
	for _, ch := range chunks {
		b = append(b, fmt.Sprintf("%04x", len(ch))...)
		b = append(b, ch...)
	}
	return append(b, "0000"...)
}

type preset struct {
	prefix string
	reply  func(conn net.Conn) error // nil: close without replying
}

type fakeServer struct {
	t       *testing.T
	ln      net.Listener
	presets []preset
	mu      sync.Mutex
	records []string // service strings, in arrival order
	raws    [][]byte // full raw packets, in arrival order
	conns   []net.Conn
	wg      sync.WaitGroup
}

func newFakeServer(t *testing.T, presets ...preset) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake server listen: %v", err)
	}
	s := &fakeServer{t: t, ln: ln, presets: presets}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.handle(conn)
	}
}

// handle reads packets and responds; records are pushed before the reply is
// written, so once the client's call returns its requests are visible.
func (s *fakeServer) handle(conn net.Conn) {
	defer conn.Close()
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n > 1<<20 {
			s.t.Errorf("fake server: oversized request (%d bytes)", n)
			return
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(conn, p); err != nil {
			return
		}
		cmd := string(p)

		s.mu.Lock()
		s.records = append(s.records, cmd)
		s.raws = append(s.raws, append(append([]byte(nil), hdr[:]...), p...))
		s.mu.Unlock()

		pr := preset{}
		found := false
		for _, cand := range s.presets {
			if strings.HasPrefix(cmd, cand.prefix) && (!found || len(cand.prefix) > len(pr.prefix)) {
				pr = cand
				found = true
			}
		}
		if !found {
			s.t.Errorf("fake server: no preset for %q", cmd)
			return
		}
		if pr.reply == nil {
			return // close without replying: raw disconnect
		}
		if err := pr.reply(conn); err != nil {
			return
		}
	}
}

func (s *fakeServer) recordsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.records...)
}

func (s *fakeServer) rawsSnapshot() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.raws))
	for i, r := range s.raws {
		out[i] = append([]byte(nil), r...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

var pseudoPNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', // magic
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, // scaled sub-streams
	0x08, 0x09, 0x0a, 0x0b, 0x0c,
}

const sampleXML = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0" width="1080" height="1920">
  <node index="0" text="" resource-id="com.douyin:id/root" class="android.widget.FrameLayout" package="com.douyin" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,0][1080,1920]">
    <node index="2" text="评论" resource-id="com.douyin:id/comment_entry" class="android.widget.TextView" package="com.douyin" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[30,900][330,1000]"/>
    <node index="3" text="" resource-id="" class="android.widget.Button" package="com.douyin" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[330,900][630,1000]">
      <node index="0" text="收藏" resource-id="" class="android.widget.TextView" package="com.douyin" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[330,900][630,1000]"/>
    </node>
  </node>
</hierarchy>
`

func connectForTest(t *testing.T, addr string) *Client {
	t.Helper()
	c, err := Connect(addr)
	if err != nil {
		t.Fatalf("Connect(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// findNode walks the tree looking for the first node whose text matches.
func findNode(n *Node, text string) *Node {
	if n == nil {
		return nil
	}
	if n.Text == text {
		return n
	}
	for _, ch := range n.Children {
		if m := findNode(ch, text); m != nil {
			return m
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Protocol-level tests
// ---------------------------------------------------------------------------

// TestSendRequestEncoding: the request on the wire is exactly
// [4-byte LE length][payload], and OKAY replies are merged segment by
// segment until the zero-length terminator.
func TestSendRequestEncoding(t *testing.T) {
	s := newFakeServer(t, preset{
		prefix: "host:",
		reply:  func(conn net.Conn) error { _, err := conn.Write(okReply([]byte("ab"), []byte("cd"))); return err },
	})
	c := connectForTest(t, s.addr())

	payload, err := c.send("host:ping")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if string(payload) != "abcd" {
		t.Fatalf("merged payload = %q, want %q", payload, "abcd")
	}

	raws := s.rawsSnapshot()
	if len(raws) != 1 {
		t.Fatalf("recorded %d packets, want 1", len(raws))
	}
	if want := enc("host:ping"); !bytes.Equal(raws[0], want) {
		t.Fatalf("raw packet = %x, want %x (length prefix must be the LE byte count)", raws[0], want)
	}
	recs := s.recordsSnapshot()
	if !equalStrings(recs, []string{"host:ping"}) {
		t.Fatalf("records = %q, want [host:ping]", recs)
	}
}

// TestSendFail: a FAIL reply surfaces its message; AUTH maps to
// ErrAuthRequired.
func TestSendFail(t *testing.T) {
	s := newFakeServer(t, preset{
		prefix: "host:",
		reply: func(conn net.Conn) error {
			_, err := conn.Write(failReply("unknown device: emulator-5554"))
			return err
		},
	})
	c := connectForTest(t, s.addr())

	if _, err := c.send("host:transport:emulator-5554"); err == nil {
		t.Fatal("expected FAIL error")
	} else if !strings.Contains(err.Error(), "unknown device") {
		t.Fatalf("error %q does not carry the server message", err)
	}
}

func TestSendAuth(t *testing.T) {
	s := newFakeServer(t, preset{
		prefix: "host:",
		reply:  func(conn net.Conn) error { _, err := conn.Write([]byte("AUTH")); return err },
	})
	c := connectForTest(t, s.addr())

	_, err := c.send("host:transport:emulator-5554")
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
}

// TestExecOutMergesHex4Frames: hex4 length frames are merged in arrival
// order; the stream may be cut into arbitrarily small frames.
func TestExecOutMergesHex4Frames(t *testing.T) {
	s := newFakeServer(t,
		preset{prefix: "host:transport:", reply: func(conn net.Conn) error { _, err := conn.Write(okReply()); return err }},
		preset{prefix: "shell:exec-out:screencap -p", reply: func(conn net.Conn) error {
			_, err := conn.Write(append([]byte("OKAY"), streamReply(
				pseudoPNG[:5], pseudoPNG[5:13], pseudoPNG[13:],
			)...))
			return err
		}},
	)
	c := connectForTest(t, s.addr())

	out, err := c.ExecOut("emulator-5554", "screencap -p")
	if err != nil {
		t.Fatalf("ExecOut: %v", err)
	}
	if !bytes.Equal(out, pseudoPNG) {
		t.Fatalf("merged output = %x, want %x", out, pseudoPNG)
	}
	// the exchange must be transport-then-execcout on the same connection
	recs := s.recordsSnapshot()
	if want := []string{"host:transport:emulator-5554", "shell:exec-out:screencap -p"}; !equalStrings(recs, want) {
		t.Fatalf("records = %q, want %q", recs, want)
	}
}

// TestExecOutKeepsArrivalOrder: chunks that would stream out of order in a
// real shell (stdout/stderr segments interleaving) are merged in the order
// they arrive.
func TestExecOutKeepsArrivalOrder(t *testing.T) {
	s := newFakeServer(t,
		preset{prefix: "host:transport:", reply: func(conn net.Conn) error { _, err := conn.Write(okReply()); return err }},
		preset{prefix: "shell:exec-out:cat /sdcard/log.txt", reply: func(conn net.Conn) error {
			// stderr-ish frame first, then stdout frames
			_, err := conn.Write(append([]byte("OKAY"), streamReply([]byte("E!"), []byte("line1\n"), []byte("line2\n"))...))
			return err
		}},
	)
	c := connectForTest(t, s.addr())

	out, err := c.ExecOut("emulator-5554", "cat /sdcard/log.txt")
	if err != nil {
		t.Fatalf("ExecOut: %v", err)
	}
	if want := "E!line1\nline2\n"; string(out) != want {
		t.Fatalf("merged output = %q, want %q", out, want)
	}
}

// TestExecOutEmptyOutputAndDisconnectErrors: zero-framed OKAY is a clean
// empty result; a frame cut mid-header or mid-data, or an AUTH status, are
// hard errors. Each scenario runs on a fresh connection: EOF-terminated
// streams end the session on the fake server, so one client cannot drive two
// ExecOut calls.
func TestExecOutEmptyOutputAndDisconnectErrors(t *testing.T) {
	s := newFakeServer(t,
		preset{prefix: "host:transport:", reply: func(conn net.Conn) error { _, err := conn.Write(okReply()); return err }},
		preset{prefix: "shell:exec-out:empty", reply: func(conn net.Conn) error {
			if _, err := conn.Write([]byte("OKAY")); err != nil {
				return err
			}
			return conn.Close() // exec-out device closes right after OKAY for empty output
		}},
		preset{prefix: "shell:exec-out:boom-header", reply: func(conn net.Conn) error {
			// OKAY then a partial 4-byte hex frame header, then disconnect
			if _, err := conn.Write([]byte("OKAY")); err != nil {
				return err
			}
			if _, err := conn.Write([]byte("00")); err != nil {
				return err
			}
			return io.ErrClosedPipe
		}},
		preset{prefix: "shell:exec-out:boom-data", reply: func(conn net.Conn) error {
			if _, err := conn.Write([]byte("OKAY")); err != nil {
				return err
			}
			if _, err := conn.Write([]byte("0004")); err != nil {
				return err
			}
			if _, err := conn.Write([]byte("a")); err != nil {
				return err
			}
			return io.ErrClosedPipe
		}},
		preset{prefix: "shell:exec-out:auth", reply: func(conn net.Conn) error {
			if _, err := conn.Write([]byte("AUTH")); err != nil {
				return err
			}
			return conn.Close()
		}},
	)
	execOut := func(serial, cmd string) ([]byte, error) {
		c := connectForTest(t, s.addr())
		return c.ExecOut(serial, cmd)
	}

	if out, err := execOut("emulator-5554", "empty"); err != nil {
		t.Fatalf("empty exec-out: %v", err)
	} else if len(out) != 0 {
		t.Fatalf("empty exec-out returned %x", out)
	}

	if _, err := execOut("emulator-5554", "boom-header"); err == nil || !strings.Contains(err.Error(), "frame") {
		t.Fatalf("boom-header err = %v, want truncated-frame error", err)
	}
	if _, err := execOut("emulator-5554", "boom-data"); err == nil || !strings.Contains(err.Error(), "frame") {
		t.Fatalf("boom-data err = %v, want truncated-frame error", err)
	}
	if _, err := execOut("emulator-5554", "auth"); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("auth err = %v, want ErrAuthRequired", err)
	}
}

// TestShellReadsUntilClose: the raw shell: stream is drained until the peer
// closes; FAIL replies surface their message. The fake server closes the
// connection after each raw shell reply (EOF-terminated stream), so the two
// Shell calls get their own clients.
func TestShellReadsUntilClose(t *testing.T) {
	s := newFakeServer(t,
		preset{prefix: "host:transport:", reply: func(conn net.Conn) error { _, err := conn.Write(okReply()); return err }},
		preset{prefix: "shell:echo", reply: func(conn net.Conn) error {
			if _, err := conn.Write([]byte("OKAY")); err != nil {
				return err
			}
			if _, err := conn.Write([]byte("total")); err != nil {
				return err
			}
			if _, err := conn.Write([]byte("\n")); err != nil {
				return err
			}
			return conn.Close()
		}},
		preset{prefix: "shell:ls", reply: func(conn net.Conn) error { _, err := conn.Write(failReply("ls: not found")); return err }},
	)

	out, err := connectForTest(t, s.addr()).Shell("emulator-5554", "echo hi")
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if out != "total\n" {
		t.Fatalf("Shell output = %q, want %q", out, "total\n")
	}

	if _, err := connectForTest(t, s.addr()).Shell("emulator-5554", "ls no-such"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Shell FAIL err = %v, want message from server", err)
	}
}

// TestTapSwipeKeyTextSequence: the fake server records the full command
// sequence; each method must select the device host:transport before its
// shell service, and the input payloads must be exactly encoded. The raw
// shell replies are EOF-terminated (the server closes after each), so every
// method call uses a fresh connection; the recorded sequence still proves
// the per-call transport-before-shell ordering.
func TestTapSwipeKeyTextSequence(t *testing.T) {
	s := newFakeServer(t,
		preset{prefix: "host:transport:", reply: func(conn net.Conn) error { _, err := conn.Write(okReply()); return err }},
		preset{prefix: "shell:", reply: func(conn net.Conn) error {
			if _, err := conn.Write([]byte("OKAY")); err != nil {
				return err
			}
			return conn.Close()
		}},
	)

	run := func(fn func(c *Client) error) {
		t.Helper()
		if err := fn(connectForTest(t, s.addr())); err != nil {
			t.Fatal(err)
		}
	}
	run(func(c *Client) error { return c.Tap("emulator-5554", 10, 20) })
	run(func(c *Client) error { return c.Swipe("emulator-5554", 0, 0, 100, 50, 300) })
	run(func(c *Client) error { return c.KeyText("emulator-5554", "hello world!") })

	want := []string{
		"host:transport:emulator-5554",
		"shell:input tap 10 20",
		"host:transport:emulator-5554",
		"shell:input swipe 0 0 100 50 300",
		"host:transport:emulator-5554",
		"shell:input text hello%sworld\\!",
	}
	if recs := s.recordsSnapshot(); !equalStrings(recs, want) {
		t.Fatalf("recorded sequence = %q, want %q", recs, want)
	}
	for i, w := range want {
		if raw := s.rawsSnapshot()[i]; !bytes.Equal(raw, enc(w)) {
			t.Fatalf("packet %d raw = %x, want %x", i, raw, enc(w))
		}
	}
}

func TestKeyTextEscaping(t *testing.T) {
	cases := map[string]string{
		"hello world!": "hello%sworld\\!",
		"a&b|c":        "a\\&b\\|c",
		"say \"hi\"":   "say%s\\\"hi\\\"",
		"plain123":     "plain123",
	}
	for in, want := range cases {
		if got := escapeInputText(in); got != want {
			t.Errorf("escapeInputText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestListDevices: host:devices reply is parsed line-wise; only
// "SERIAL\tdevice" entries survive, and multi-segment OKAY payloads merge.
func TestListDevices(t *testing.T) {
	seg1 := "emulator-5554\tdevice\n127.0.0.1:16416\toffline\n"
	seg2 := "192.168.1.9:5555\tdevice\n"
	s := newFakeServer(t, preset{
		prefix: "host:devices",
		reply:  func(conn net.Conn) error { _, err := conn.Write(okReply([]byte(seg1), []byte(seg2))); return err },
	})
	c := connectForTest(t, s.addr())

	got, err := c.ListDevices(s.addr())
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	want := []string{"emulator-5554", "192.168.1.9:5555"}
	if !equalStrings(got, want) {
		t.Fatalf("devices = %q, want %q", got, want)
	}

	recs := s.recordsSnapshot()
	if !equalStrings(recs, []string{"host:devices"}) {
		t.Fatalf("records = %q, want [host:devices]", recs)
	}
}

// TestUIDumpParsesNodeTree: the sampled window_dump.xml is parsed into a
// node tree; bounds are exposed and centers computed.
func TestUIDumpParsesNodeTree(t *testing.T) {
	s := newFakeServer(t,
		preset{prefix: "host:transport:", reply: func(conn net.Conn) error { _, err := conn.Write(okReply()); return err }},
		preset{prefix: "shell:exec-out:cat /sdcard/window_dump.xml", reply: func(conn net.Conn) error {
			_, err := conn.Write(append([]byte("OKAY"), streamReply([]byte(sampleXML))...))
			return err
		}},
	)
	c := connectForTest(t, s.addr())

	tree, err := c.UIDump("emulator-5554")
	if err != nil {
		t.Fatalf("UIDump: %v", err)
	}
	if tree.Root.Class != "android.widget.FrameLayout" {
		t.Fatalf("root class = %q", tree.Root.Class)
	}

	comment := findNode(tree.Root, "评论")
	if comment == nil {
		t.Fatal("comment node not found")
	}
	if comment.ResourceID != "com.douyin:id/comment_entry" {
		t.Fatalf("comment resource-id = %q", comment.ResourceID)
	}
	if comment.Bounds != "[30,900][330,1000]" {
		t.Fatalf("comment bounds = %q", comment.Bounds)
	}
	x, y, err := ClickCenter(comment)
	if err != nil {
		t.Fatalf("ClickCenter(comment): %v", err)
	}
	if x != 180 || y != 950 {
		t.Fatalf("ClickCenter = (%d,%d), want (180,950)", x, y)
	}

	// nested node parsing
	if fav := findNode(tree.Root, "收藏"); fav == nil || len(tree.Root.Children) < 1 {
		t.Fatal("nested node not parsed")
	}
}

// TestUIDumpBadXML: garbage from the cat service is a parse error.
func TestUIDumpBadXML(t *testing.T) {
	s := newFakeServer(t,
		preset{prefix: "host:transport:", reply: func(conn net.Conn) error { _, err := conn.Write(okReply()); return err }},
		preset{prefix: "shell:exec-out:cat /sdcard/window_dump.xml", reply: func(conn net.Conn) error {
			_, err := conn.Write(append([]byte("OKAY"), streamReply([]byte("<not-xml"))...))
			return err
		}},
	)
	c := connectForTest(t, s.addr())

	if _, err := c.UIDump("emulator-5554"); err == nil {
		t.Fatal("expected parse error for garbage XML")
	}
}

// TestParseBoundsAndCenter: attribute parsing and center computation.
func TestParseBoundsAndCenter(t *testing.T) {
	b, err := ParseBounds("[30,900][330,1000]")
	if err != nil {
		t.Fatalf("ParseBounds: %v", err)
	}
	if b != (Bounds{X1: 30, Y1: 900, X2: 330, Y2: 1000}) {
		t.Fatalf("bounds = %+v", b)
	}
	x, y := b.Center()
	if x != 180 || y != 950 {
		t.Fatalf("center = (%d,%d)", x, y)
	}

	for _, bad := range []string{"", "30,900", "[0,0]", "[0,0][0,0]x", "[5,6][1,2]"} {
		if _, err := ParseBounds(bad); err == nil {
			t.Errorf("ParseBounds(%q): want error", bad)
		}
	}
}

// TestParseNodeTreeStripsDumpHint: the "UI hierchary dumped to: ..." hint
// line (`uiautomator dump` prints it before the XML) is tolerated.
func TestParseNodeTreeStripsDumpHint(t *testing.T) {
	blob := "UI hierchary dumped to: /sdcard/window_dump.xml\n" + sampleXML
	tree, err := ParseNodeTree([]byte(blob))
	if err != nil {
		t.Fatalf("ParseNodeTree with hint: %v", err)
	}
	if tree.Root == nil || tree.Root.Class != "android.widget.FrameLayout" {
		t.Fatalf("root = %+v", tree.Root)
	}

	if p, ok := uiDumpPath([]byte(blob)); !ok || p != "/sdcard/window_dump.xml" {
		t.Fatalf("uiDumpPath = %q, %v", p, ok)
	}
	if _, ok := uiDumpPath([]byte(sampleXML)); ok {
		t.Fatal("uiDumpPath must reject XML without a hint line")
	}
	if _, ok := uiDumpPath([]byte("UI hierchary dumped to:\n")); ok {
		t.Fatal("uiDumpPath must reject an empty path")
	}
}

// TestConnectRefused: dialing a dead server address errors out.
func TestConnectRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if c, err := Connect(addr); err == nil {
		_ = c.Close()
		t.Fatal("Connect to closed listener: want error")
	}
}

// TestOpAfterServerDisconnect: operations against a pile-up that closes the
// connection mid-exchange must not hang and must surface an error.
func TestOpAfterServerDisconnect(t *testing.T) {
	s := newFakeServer(t,
		preset{prefix: "host:transport:", reply: func(conn net.Conn) error { _, err := conn.Write(okReply()); return err }},
		preset{prefix: "shell:", reply: nil}, // immediate disconnect
	)
	c := connectForTest(t, s.addr())

	if out, err := c.Shell("emulator-5554", "input keyevent 4"); err == nil {
		t.Fatalf("Shell on disconnected server: out=%q, want error", out)
	}
	if err := c.Tap("emulator-5554", 1, 2); err == nil {
		t.Fatal("Tap on disconnected server: want error")
	}
}

// TestClosedClient: methods on a closed client fail fast. The fake server is
// guaranteed up, so Connect must succeed.
func TestClosedClient(t *testing.T) {
	s := newFakeServer(t, preset{prefix: "host:", reply: func(conn net.Conn) error { _, err := conn.Write(okReply()); return err }})
	c, err := Connect(s.addr())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.ExecOut("emulator-5554", "screencap -p"); err == nil {
		t.Fatal("ExecOut on closed client: want error")
	}
}

// TestListDevicesDeadline: ListDevices against a listener that accepts but
// never replies must time out rather than hang forever.
func TestListDevicesDeadline(t *testing.T) {
	old := listDevicesTimeout
	listDevicesTimeout = time.Second
	t.Cleanup(func() { listDevicesTimeout = old })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()

	c := connectForTest(t, ln.Addr().String())
	start := time.Now()
	if _, err := c.ListDevices(ln.Addr().String()); err == nil {
		t.Fatal("ListDevices on a mute server: want timeout error")
	} else if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("ListDevices err = %v, want a deadline error", err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("ListDevices took %s, deadline not honored", el)
	}
}
