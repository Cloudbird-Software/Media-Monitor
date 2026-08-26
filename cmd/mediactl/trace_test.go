package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/trace"
)

// fakeAdbServer is a minimal adb-server wire twin for the trace wiring test:
// hex4 request framing, OKAY replies, hex4-framed exec/list output. It
// records every received request in order for assertions.
type fakeAdbServer struct {
	mu  sync.Mutex
	got []string
}

func (f *fakeAdbServer) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// startFakeAdbServer launches the fake server and returns its address.
func startFakeAdbServer(t *testing.T) (string, *fakeAdbServer) {
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
					s := string(req)
					fs.mu.Lock()
					fs.got = append(fs.got, s)
					fs.mu.Unlock()
					switch {
					case s == "host:devices":
						_, _ = bw.WriteString("OKAY")
						writeAdbHexFrames(bw, "ser-1\tdevice\n")
					case strings.HasPrefix(s, "host:transport:"):
						_, _ = bw.WriteString("OKAY")
					case strings.HasPrefix(s, "exec:cat"):
						_, _ = bw.WriteString("OKAY")
						writeAdbHexFrames(bw, `<?xml version="1.0" encoding="UTF-8"?><hierarchy rotation="0"><node text="主页" bounds="[0,0][100,100]"/></hierarchy>`)
					case strings.HasPrefix(s, "exec:"):
						_, _ = bw.WriteString("OKAY")
						writeAdbHexFrames(bw)
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

func writeAdbHexFrames(w io.Writer, parts ...string) {
	for _, p := range parts {
		if p == "" {
			continue
		}
		_, _ = fmt.Fprintf(w, "%04x%s", len(p), p)
	}
	_, _ = w.Write([]byte("0000"))
}

// TestTraceRunWiresProfileURLTemplate runs trace end to end against a fake
// adb server and asserts the profile deep link came from the flow's
// profile_url_template (not hardcoded).
func TestTraceRunWiresProfileURLTemplate(t *testing.T) {
	addr, fs := startFakeAdbServer(t)
	t.Setenv("MEDIAMON_LICENSE_REQUIRED", "false")

	// A tiny flow: like always fires (no layout bounds => non-fatal skip on
	// the gesture), dwell waits 1ms. The template is what we assert on.
	flowFile := filepath.Join(t.TempDir(), "flow.json")
	flow := `{"platform":"douyin","profile_url_template":"snssdk1128://user/profile/{sec_uid}",
		"actions":[{"type":"like","prob":1},{"type":"dwell","prob":1,"duration_ms":[1,1]}]}`
	if err := os.WriteFile(flowFile, []byte(flow), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return traceRun([]string{"--platform", "douyin", "--targets", "SEC1", "--flow", flowFile, "--adb", addr})
	})
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Platform string `json:"platform"`
		Results  []struct {
			Target string `json:"target"`
			Device string `json:"device"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, out)
	}
	if rep.Platform != "douyin" || len(rep.Results) != 1 || rep.Results[0].Target != "SEC1" || rep.Results[0].Device != "ser-1" {
		t.Fatalf("report = %+v", rep)
	}

	var sawOpen, sawBack bool
	for _, r := range fs.requests() {
		if strings.Contains(r, "am start -a android.intent.action.VIEW -d 'snssdk1128://user/profile/SEC1'") {
			sawOpen = true
		}
		if strings.Contains(r, "input keyevent KEYCODE_BACK") {
			sawBack = true
		}
	}
	if !sawOpen {
		t.Fatalf("adb never saw the flow-templated profile open; requests = %v", fs.requests())
	}
	if !sawBack {
		t.Fatalf("adb never saw the release back key; requests = %v", fs.requests())
	}
}

// TestTraceRunMissingTemplateFailsClosed: a flow without profile_url_template
// is rejected before any adb traffic.
func TestTraceRunMissingTemplateFailsClosed(t *testing.T) {
	t.Setenv("MEDIAMON_LICENSE_REQUIRED", "false")
	flowFile := filepath.Join(t.TempDir(), "flow.json")
	if err := os.WriteFile(flowFile, []byte(`{"platform":"douyin","actions":[{"type":"like","prob":1}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := traceRun([]string{"--platform", "douyin", "--targets", "SEC1", "--flow", flowFile, "--adb", "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "profile_url_template") {
		t.Fatalf("error = %v, want profile_url_template fail-closed", err)
	}
	if _, _, err := traceGestureExecutor("127.0.0.1:1", mustLoadFlow(t, flowFile)); err == nil {
		t.Fatal("traceGestureExecutor accepted an empty template")
	}
}

// TestTraceDefaultFlowTemplate: the shipped douyin flow loads via the
// platform convention and renders its profile URL.
func TestTraceDefaultFlowTemplate(t *testing.T) {
	t.Setenv("MEDIAMON_ADAPT_DIR", filepath.Join("..", "..", "adapt"))
	flow, err := loadTraceFlow("", "douyin")
	if err != nil {
		t.Fatal(err)
	}
	u, err := flow.RenderProfileURL("SEC1")
	if err != nil {
		t.Fatal(err)
	}
	if u != "snssdk1128://user/profile/SEC1" {
		t.Fatalf("rendered = %q", u)
	}
}

func mustLoadFlow(t *testing.T, path string) trace.Flow {
	t.Helper()
	fl, err := loadTraceFlow(path, "douyin")
	if err != nil {
		t.Fatal(err)
	}
	return fl
}
