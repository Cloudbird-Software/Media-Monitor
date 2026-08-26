package trace

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adb"
)

// fakeAdbServer is a minimal adb-server wire twin for executor tests: hex4
// request framing, OKAY replies, hex4-framed exec output. It records every
// received request in order for sequence assertions.
type fakeAdbServer struct {
	mu  sync.Mutex
	got []string
}

func (f *fakeAdbServer) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// startFakeAdb launches the fake server and returns its address. Each
// connection serves transport/exec requests until the peer goes away.
func startFakeAdb(t *testing.T) (string, *fakeAdbServer) {
	t.Helper()
	fs := &fakeAdbServer{}
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
					var n int
					if _, err := fmt.Sscanf(string(hdr[:]), "%04x", &n); err != nil || n == 0 {
						return
					}
					req := make([]byte, n)
					if _, err := io.ReadFull(br, req); err != nil {
						return
					}
					fs.mu.Lock()
					fs.got = append(fs.got, string(req))
					fs.mu.Unlock()
					s := string(req)
					switch {
					case strings.HasPrefix(s, "host:transport:"):
						_, _ = bw.WriteString("OKAY")
					case strings.HasPrefix(s, "exec:uiautomator"):
						_, _ = bw.WriteString("OKAY")
						writeAdbFrames(bw, "UI hierchary dumped to: /sdcard/window_dump.xml")
					case strings.HasPrefix(s, "exec:cat"):
						_, _ = bw.WriteString("OKAY")
						writeAdbFrames(bw, `<?xml version="1.0" encoding="UTF-8"?><hierarchy rotation="0"><node text="主页" class="android.widget.TextView" bounds="[0,0][100,100]" clickable="true"/></hierarchy>`)
					case strings.HasPrefix(s, "exec:"):
						_, _ = bw.WriteString("OKAY")
						writeAdbFrames(bw)
					default:
						return
					}
					_ = bw.Flush()
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), fs
}

func writeAdbFrames(w io.Writer, parts ...string) {
	for _, p := range parts {
		if p == "" {
			continue
		}
		_, _ = fmt.Fprintf(w, "%04x%s", len(p), p)
	}
	_, _ = w.Write([]byte("0000"))
}

// newFakeExecutor builds an AdbExecutor whose factory dials the fake adb
// server (a fresh connection per factory call, mirroring per-service adb
// sessions).
func newFakeExecutor(t *testing.T, addr string, layout Layout, opts ...ExecutorOption) *AdbExecutor {
	t.Helper()
	return AdbExecutorBy(func(serial string) (adbClient, error) {
		c, err := adb.Connect(addr)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = c.Close() })
		return &adbClientWrapper{c: c}, nil
	}, layout, opts...)
}

// TestPrepareMissingTemplateFailsClosed: without a flow-declared
// profile_url_template the executor must refuse to open anything.
func TestPrepareMissingTemplateFailsClosed(t *testing.T) {
	addr, fs := startFakeAdb(t)
	exec := newFakeExecutor(t, addr, Layout{})
	err := exec.Prepare(context.Background(), Device{Serial: "ser-1"}, Target{SecUID: "SEC1"})
	if err == nil || !strings.Contains(err.Error(), "profile_url_template") {
		t.Fatalf("expected fail-closed template error, got %v", err)
	}
	if got := fs.requests(); len(got) != 0 {
		t.Fatalf("no adb command must be sent without a template, got %v", got)
	}
}

// TestRenderProfileURL: template rendering is exact, and a template without
// the {sec_uid} placeholder is rejected.
func TestRenderProfileURL(t *testing.T) {
	f := Flow{ProfileURLTemplate: "snssdk1128://user/profile/{sec_uid}"}
	got, err := f.RenderProfileURL("MS4wLjABAAAA")
	if err != nil {
		t.Fatal(err)
	}
	if got != "snssdk1128://user/profile/MS4wLjABAAAA" {
		t.Fatalf("rendered = %q", got)
	}
	if _, err := (Flow{}).RenderProfileURL("x"); err == nil {
		t.Fatal("empty template must fail closed")
	}
	if _, err := (Flow{ProfileURLTemplate: "scheme://noplaceholder"}).RenderProfileURL("x"); err == nil {
		t.Fatal("template without {sec_uid} must fail closed")
	}
}

// TestAdbExecutorWireSequence: Prepare renders the flow deep link and issues
// am start + a uiautomator probe; gesture actions dispatch tap/swipe/text;
// Release presses back — all as real adb wire requests in order.
func TestAdbExecutorWireSequence(t *testing.T) {
	addr, fs := startFakeAdb(t)
	layout := Layout{
		ActionLike:    {100, 200, 300, 400}, // center 200,300
		ActionComment: {10, 20, 30, 40},     // center 20,30
		ActionDwell:   {0, 0, 500, 500},     // center 250,250
	}
	exec := newFakeExecutor(t, addr, layout, WithProfileURLTemplate("snssdk1128://user/profile/{sec_uid}"))
	dev := Device{Serial: "ser-1"}
	tgt := Target{SecUID: "SEC-42", Payload: map[string]any{"comment": "hello world"}}
	ctx := context.Background()

	if err := exec.Prepare(ctx, dev, tgt); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, _, err := exec.Run(ctx, dev, tgt, Action{Type: ActionLike, Prob: 1}); err != nil {
		t.Fatalf("Run like: %v", err)
	}
	if _, _, err := exec.Run(ctx, dev, tgt, Action{Type: ActionComment, Prob: 1}); err != nil {
		t.Fatalf("Run comment: %v", err)
	}
	if _, _, err := exec.Run(ctx, dev, tgt, Action{Type: ActionDwell, Prob: 1}); err != nil {
		t.Fatalf("Run dwell: %v", err)
	}
	if err := exec.Release(ctx, dev, tgt); err != nil {
		t.Fatalf("Release: %v", err)
	}

	got := fs.requests()
	var cmds []string
	for _, r := range got {
		if strings.HasPrefix(r, "host:transport:ser-1") {
			continue // transport binds are bookkeeping, not actions
		}
		cmds = append(cmds, r)
	}
	want := []string{
		"exec:am start -a android.intent.action.VIEW -d 'snssdk1128://user/profile/SEC-42'",
		"exec:uiautomator dump /sdcard/window_dump.xml",
		"exec:cat /sdcard/window_dump.xml",
		"exec:input tap 200 300",               // like center
		"exec:input tap 20 30",                 // comment entry tap
		"exec:input text hello%sworld",         // comment text (spaces -> %s)
		"exec:input swipe 250 250 250 450 200", // dwell swipe down 200px
		"exec:input keyevent KEYCODE_BACK",
	}
	if strings.Join(cmds, "|") != strings.Join(want, "|") {
		t.Fatalf("wire sequence mismatch:\n got: %v\nwant: %v", cmds, want)
	}
}

// TestAdbExecutorNoLayoutSkipsNonFatal: an action without declared bounds is
// a non-fatal skip and sends nothing.
func TestAdbExecutorNoLayoutSkipsNonFatal(t *testing.T) {
	addr, fs := startFakeAdb(t)
	exec := newFakeExecutor(t, addr, Layout{}, WithProfileURLTemplate("x://{sec_uid}"))
	_, nonFatal, err := exec.Run(context.Background(), Device{Serial: "s"}, Target{SecUID: "t"}, Action{Type: ActionFollow, Prob: 1})
	if err != nil || !nonFatal {
		t.Fatalf("expected non-fatal skip, got nonFatal=%v err=%v", nonFatal, err)
	}
	if got := fs.requests(); len(got) != 0 {
		t.Fatalf("no wire traffic expected for a skipped action, got %v", got)
	}
}

// TestAdbExecutorPrepareBadTemplateNoTraffic: a template without the
// placeholder fails closed before any wire traffic.
func TestAdbExecutorPrepareBadTemplateNoTraffic(t *testing.T) {
	addr, fs := startFakeAdb(t)
	exec := newFakeExecutor(t, addr, Layout{}, WithProfileURLTemplate("scheme://fixed"))
	if err := exec.Prepare(context.Background(), Device{Serial: "s"}, Target{SecUID: "t"}); err == nil {
		t.Fatal("expected placeholder error")
	}
	if got := fs.requests(); len(got) != 0 {
		t.Fatalf("no wire traffic expected, got %v", got)
	}
}
