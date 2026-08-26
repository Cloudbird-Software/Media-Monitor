package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adb"
	"github.com/Cloudbird-Software/Media-Monitor/internal/vision"
)

// fakeDevice records every adb-shaped call (deviceOps surface).
type fakeDevice struct {
	mu      sync.Mutex
	taps    [][2]int32
	swipes  []string
	texts   []string
	shells  []string
	screens int
	uidumps int
	uidTree *adb.NodeTree
	tapErr  error
}

func (f *fakeDevice) Tap(x, y int32) error {
	f.mu.Lock()
	f.taps = append(f.taps, [2]int32{x, y})
	f.mu.Unlock()
	return f.tapErr
}
func (f *fakeDevice) Swipe(x0, y0, x1, y1 int32, ms int) error {
	f.mu.Lock()
	f.swipes = append(f.swipes, fmt.Sprintf("%d,%d->%d,%d@%d", x0, y0, x1, y1, ms))
	f.mu.Unlock()
	return nil
}
func (f *fakeDevice) KeyText(t string) error {
	f.mu.Lock()
	f.texts = append(f.texts, t)
	f.mu.Unlock()
	return nil
}
func (f *fakeDevice) ScreencapPNG() ([]byte, error) {
	f.mu.Lock()
	f.screens++
	f.mu.Unlock()
	return []byte("png"), nil
}
func (f *fakeDevice) Shell(cmd string) (string, error) {
	f.mu.Lock()
	f.shells = append(f.shells, cmd)
	f.mu.Unlock()
	return "ok", nil
}
func (f *fakeDevice) UIDump() (*adb.NodeTree, error) {
	f.mu.Lock()
	f.uidumps++
	f.mu.Unlock()
	return f.uidTree, nil
}

// TestVisionEndpointFailClosed: MEDIAMON_VISION_ENDPOINT unset (or blank)
// is an explicit error — never a silent skip (W6-C1 AC-3).
func TestVisionEndpointFailClosed(t *testing.T) {
	t.Setenv("MEDIAMON_VISION_ENDPOINT", "")
	if _, err := visionProviderFromEnv(); err == nil || !strings.Contains(err.Error(), visionEnv) {
		t.Fatalf("err = %v, want explicit %s-required error", err, visionEnv)
	}
	t.Setenv("MEDIAMON_VISION_ENDPOINT", "   ")
	if _, err := visionProviderFromEnv(); err == nil {
		t.Fatal("blank endpoint must also fail closed")
	}
}

// TestAdbExecutorSemanticMapping: tap / swipe / type_text each land on
// exactly one adb primitive with the right parameters; screencap powers the
// observation; ui_action resolves a hint through uidump and taps the node
// center (W6-C1 AC-2).
func TestAdbExecutorSemanticMapping(t *testing.T) {
	dev := &fakeDevice{}
	ex := adbExecutor{dev: dev}

	if out, err := ex.Execute(vision.Action{Type: vision.ActionTap, Args: map[string]any{"x": 12.0, "y": 34.0}}); err != nil || out != "tap" {
		t.Fatalf("tap: %v %q", err, out)
	}
	dev.mu.Lock()
	taps := append([][2]int32(nil), dev.taps...)
	dev.mu.Unlock()
	if len(taps) != 1 || taps[0] != [2]int32{12, 34} {
		t.Fatalf("taps = %v", taps)
	}

	if _, err := ex.Execute(vision.Action{Type: vision.ActionSwipe, Args: map[string]any{"x0": 1.0, "y0": 2.0, "x1": 3.0, "y1": 4.0, "duration_ms": 250.0}}); err != nil {
		t.Fatal(err)
	}
	dev.mu.Lock()
	sw := append([]string(nil), dev.swipes...)
	dev.mu.Unlock()
	if len(sw) != 1 || sw[0] != "1,2->3,4@250" {
		t.Fatalf("swipes = %v", sw)
	}

	if _, err := ex.Execute(vision.Action{Type: vision.ActionTypeText, Args: map[string]any{"text": "hello"}}); err != nil {
		t.Fatal(err)
	}
	dev.mu.Lock()
	tx := append([]string(nil), dev.texts...)
	dev.mu.Unlock()
	if len(tx) != 1 || tx[0] != "hello" {
		t.Fatalf("texts = %v", tx)
	}

	if _, err := ex.Screenshot(); err != nil {
		t.Fatal(err)
	}
	dev.mu.Lock()
	sc := dev.screens
	dev.mu.Unlock()
	if sc != 1 {
		t.Fatalf("screencap calls = %d", sc)
	}

	// ui_action: hint resolves via uidump to the node center.
	dev.uidTree = &adb.NodeTree{Root: &adb.Node{Text: "Search", Bounds: "[10,20][110,120]", Children: []*adb.Node{}}}
	if _, err := ex.Execute(vision.Action{Type: vision.ActionUIAction, Args: map[string]any{"hint": "search"}}); err != nil {
		t.Fatal(err)
	}
	dev.mu.Lock()
	taps = append([][2]int32(nil), dev.taps...)
	du := dev.uidumps
	dev.mu.Unlock()
	if du != 1 || len(taps) != 2 || taps[1] != [2]int32{60, 70} {
		t.Fatalf("ui_action: uidumps=%d taps=%v", du, taps)
	}
}

// mockVisionEndpoint serves a scripted action sequence (one action per
// request) as an OpenAI-compatible chat completion.
func mockVisionEndpoint(t *testing.T, actions []vision.Action) *httptest.Server {
	t.Helper()
	i := 0
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := i
		i++
		mu.Unlock()
		var act vision.Action
		if idx < len(actions) {
			act = actions[idx]
		} else {
			act = vision.Action{Type: vision.ActionDone}
		}
		content, _ := json.Marshal(act)
		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": string(content)}}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestVisionRunEndToEnd: mock endpoint + mock device complete the
// open-target → observe → locate → tap → done chain (W6-C1 AC-5) and the
// successful run distills into a flow script on disk (AC-4).
func TestVisionRunEndToEnd(t *testing.T) {
	srv := mockVisionEndpoint(t, []vision.Action{
		{Type: vision.ActionUIAction, Args: map[string]any{"hint": "search"}, Reason: "locate search box"},
		{Type: vision.ActionTypeText, Args: map[string]any{"text": "keyword"}, Reason: "type"},
		{Type: vision.ActionTap, Args: map[string]any{"x": 50.0, "y": 50.0}, Reason: "first result"},
	})
	defer srv.Close()
	t.Setenv("MEDIAMON_VISION_ENDPOINT", srv.URL)
	p, err := visionProviderFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dev := &fakeDevice{uidTree: &adb.NodeTree{Root: &adb.Node{Text: "Search", Bounds: "[0,0][100,100]", Children: []*adb.Node{}}}}
	distill := filepath.Join(t.TempDir(), "flow.json")
	turns, err := visionRun(context.Background(), p, adbExecutor{dev: dev}, "open search and run a query", 10, distill)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 4 { // 3 actions + done
		t.Fatalf("turns = %d, want 4", len(turns))
	}
	dev.mu.Lock()
	screens := dev.screens
	dev.mu.Unlock()
	if screens != 4 {
		t.Fatalf("observe loop screencaps = %d, want 4 (one per step)", screens)
	}
	raw, err := os.ReadFile(distill)
	if err != nil {
		t.Fatalf("distilled flow missing: %v", err)
	}
	var flow vision.FlowScript
	if err := json.Unmarshal(raw, &flow); err != nil {
		t.Fatal(err)
	}
	if len(flow.Steps) != 4 || flow.Steps[0].Action.Type != vision.ActionUIAction {
		t.Fatalf("distilled flow steps = %+v", flow.Steps)
	}
}

// TestVisionRunStepBudget: a never-done provider stops at the budget with
// an explicit error (no hang, INV-2).
func TestVisionRunStepBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		act, _ := json.Marshal(vision.Action{Type: vision.ActionTap, Args: map[string]any{"x": 1.0, "y": 1.0}})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": string(act)}}},
		})
	}))
	defer srv.Close()
	p := vision.NewOpenAICompat(vision.OpenAICompat{Endpoint: srv.URL})
	dev := &fakeDevice{}
	_, err := visionRun(context.Background(), p, adbExecutor{dev: dev}, "unreachable", 3, "")
	if err == nil || !strings.Contains(err.Error(), "3 steps") {
		t.Fatalf("err = %v, want budget exhausted", err)
	}
}

// TestAdbExecutorPromptKeyAliases: the provider prompt's own key names
// (node_hint / ms) drive the bridge exactly like the canonical keys — a
// schema-faithful endpoint must never dead-end (holdout H6 regression).
func TestAdbExecutorPromptKeyAliases(t *testing.T) {
	dev := &fakeDevice{uidTree: &adb.NodeTree{Root: &adb.Node{Text: "Search", Bounds: "[0,0][100,100]", Children: []*adb.Node{}}}}
	ex := adbExecutor{dev: dev}
	if _, err := ex.Execute(vision.Action{Type: vision.ActionUIAction, Args: map[string]any{"node_hint": "search"}}); err != nil {
		t.Fatalf("node_hint alias: %v", err)
	}
	if _, err := ex.Execute(vision.Action{Type: vision.ActionSwipe, Args: map[string]any{"x0": 1, "y0": 2, "x1": 3, "y1": 4, "ms": 120}}); err != nil {
		t.Fatalf("ms alias: %v", err)
	}
	dev.mu.Lock()
	sw := append([]string(nil), dev.swipes...)
	dev.mu.Unlock()
	if len(sw) != 1 || sw[0] != "1,2->3,4@120" {
		t.Fatalf("swipes = %v (ms alias must win over the 300 default)", sw)
	}
}
