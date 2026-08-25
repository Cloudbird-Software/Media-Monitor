package vision

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recExecutor records every Execute call and can inject a failure at a
// 1-based step index (0 = never).
type recExecutor struct {
	mu     sync.Mutex
	calls  []Action
	failAt int
}

func (r *recExecutor) Exec(cmd string, args ...string) (string, error) { return "", nil }
func (r *recExecutor) Screenshot() ([]byte, error)                     { return nil, nil }

func (r *recExecutor) Execute(a Action) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, a)
	if r.failAt > 0 && len(r.calls) >= r.failAt {
		return "", errors.New("injected failure")
	}
	return "ok", nil
}

func (r *recExecutor) actions() []Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Action(nil), r.calls...)
}

// writeFlowFile writes a FlowScript JSON into a temp dir.
func writeFlowFile(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flow.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write flow: %v", err)
	}
	return path
}

func validFlowDoc() string {
	return `{
  "name": "douyin-comments-open",
  "platform": "douyin",
  "app_version": "1.2.3",
  "steps": [
    {"action": {"type": "tap", "args": {"x": 100, "y": 200}, "reason": "r"}, "expect": "评论图标", "label": "open-comments"},
    {"action": {"type": "done", "args": null}, "label": "finish"}
  ]
}`
}

// TestLoadFlowAndValidate: a well-formed flow file loads and validates.
func TestLoadFlowAndValidate(t *testing.T) {
	path := writeFlowFile(t, validFlowDoc())
	f, err := LoadFlow(path)
	if err != nil {
		t.Fatalf("LoadFlow: %v", err)
	}
	if f.Name != "douyin-comments-open" || f.Platform != "douyin" || f.AppVersion != "1.2.3" {
		t.Fatalf("flow header = %+v", f)
	}
	if len(f.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(f.Steps))
	}
	if f.Steps[0].Action.Type != ActionTap || f.Steps[0].Action.Args["x"] != float64(100) {
		t.Fatalf("step 0 = %+v", f.Steps[0])
	}
	if f.Steps[0].Expect != "评论图标" || f.Steps[0].Label != "open-comments" {
		t.Fatalf("step 0 meta = %+v", f.Steps[0])
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestLoadFlowMissing: a missing file is an error.
func TestLoadFlowMissing(t *testing.T) {
	if _, err := LoadFlow(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing flow file")
	}
}

// TestLoadFlowBadJSON: malformed JSON is an error.
func TestLoadFlowBadJSON(t *testing.T) {
	path := writeFlowFile(t, `{"name":`)
	if _, err := LoadFlow(path); err == nil {
		t.Fatal("expected error for malformed flow JSON")
	}
}

// TestValidateRejectsBrokenFlows: structural violations are each rejected.
func TestValidateRejectsBrokenFlows(t *testing.T) {
	step := `{"action": {"type": "tap", "args": {"x": 1, "y": 2}}}`
	cases := map[string]string{
		"nil flow":      "",
		"empty name":    `{"name": "  ", "app_version": "1", "steps": [` + step + `]}`,
		"empty version": `{"name": "n", "app_version": " ", "steps": [` + step + `]}`,
		"no steps":      `{"name": "n", "app_version": "1"}`,
		"bad type":      `{"name": "n", "app_version": "1", "steps": [{"action": {"type": "fly"}}]}`,
		"empty type":    `{"name": "n", "app_version": "1", "steps": [{"action": {"args": {}}}]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			var f *FlowScript
			if doc != "" {
				var parsed FlowScript
				if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
					t.Fatalf("case doc invalid: %v", err)
				}
				f = &parsed
			}
			if err := f.Validate(); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
	var f *FlowScript
	if err := f.Validate(); err == nil {
		t.Fatal("expected validation error for nil flow")
	}
}

// actionJSON builds an action doc for the JSON-derived distill test.
func actionJSON(t ActionType, args string) string {
	return `{"type":"` + t + `","args":` + args + `}`
}

// TestDistillFiltersAndDeduplicates: only succeeded steps survive,
// consecutive identical type+args collapse to the first occurrence, and
// observed text becomes Expect truncated to 40 runes.
func TestDistillFiltersAndDeduplicates(t *testing.T) {
	longObserved := strings.Repeat("观", 60)
	doc := `[
  {"action": ` + actionJSON(ActionTap, `{"x":1,"y":2}`) + `, "observed": "tapped", "succeeded": true},
  {"action": ` + actionJSON(ActionTap, `{"x":1,"y":2}`) + `, "observed": "again", "succeeded": true},
  {"action": ` + actionJSON(ActionSwipe, `{"x0":0}`) + `, "succeeded": false},
  {"action": ` + actionJSON(ActionTap, `{"x":3,"y":4}`) + `, "observed": "` + longObserved + `", "succeeded": true},
  {"action": ` + actionJSON(ActionTap, `{"x":1,"y":2}`) + `, "observed": "back", "succeeded": true},
  {"action": ` + actionJSON(ActionTap, `{"x":1,"y":2}`) + `, "succeeded": false}
]`
	var log []StepLog
	if err := json.Unmarshal([]byte(doc), &log); err != nil {
		t.Fatalf("decode log: %v", err)
	}

	f := Distill("distilled", "douyin", "9.9.9", log)
	if f.Name != "distilled" || f.Platform != "douyin" || f.AppVersion != "9.9.9" {
		t.Fatalf("header = %+v", f)
	}
	if len(f.Steps) != 3 {
		t.Fatalf("steps = %d, want 3: %+v", len(f.Steps), f.Steps)
	}
	if f.Steps[0].Action.Args["x"] != float64(1) || f.Steps[0].Expect != "tapped" {
		t.Fatalf("step 0 = %+v (dup of step 0 must be dropped, first kept)", f.Steps[0])
	}
	if f.Steps[1].Action.Args["x"] != float64(3) {
		t.Fatalf("step 1 = %+v", f.Steps[1])
	}
	if f.Steps[1].Expect != strings.Repeat("观", 40) {
		t.Fatalf("step 1 Expect = %q (len %d), want 40 runes", f.Steps[1].Expect, len([]rune(f.Steps[1].Expect)))
	}
	if f.Steps[2].Action.Args["x"] != float64(1) {
		t.Fatalf("step 2 = %+v (non-consecutive duplicate must be kept)", f.Steps[2])
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("distilled flow must validate: %v", err)
	}
}

// TestDistillEmptyLog: an empty or all-failed log yields an empty flow.
func TestDistillEmptyLog(t *testing.T) {
	f := Distill("n", "p", "1", nil)
	if len(f.Steps) != 0 {
		t.Fatalf("steps = %d, want 0", len(f.Steps))
	}
	f = Distill("n", "p", "1", []StepLog{
		{Action: Action{Type: ActionTap}, Succeeded: false},
	})
	if len(f.Steps) != 0 {
		t.Fatalf("steps = %d, want 0 after all-failed log", len(f.Steps))
	}
}

// TestDistillDoesNotAliasArgs: mutating a log's args after distillation must
// not leak into the distilled flow.
func TestDistillDoesNotAliasArgs(t *testing.T) {
	log := []StepLog{
		{Action: Action{Type: ActionTap, Args: map[string]any{"x": 1}}, Succeeded: true},
	}
	f := Distill("n", "p", "1", log)
	log[0].Action.Args["x"] = 999
	if f.Steps[0].Action.Args["x"] != 1 {
		t.Fatalf("distilled args aliased the source log: %v", f.Steps[0].Action.Args)
	}
}

// TestRunFlowSequence: steps execute in order and the executor sees exactly
// the flow's actions.
func TestRunFlowSequence(t *testing.T) {
	f := &FlowScript{Name: "seq", AppVersion: "1", Steps: []FlowStep{
		{Action: Action{Type: ActionTap, Args: map[string]any{"x": 10}}, Label: "a"},
		{Action: Action{Type: ActionTypeText, Args: map[string]any{"text": "hi"}}, Label: "b"},
		{Action: Action{Type: ActionDone}, Label: "finish"},
	}}
	ex := &recExecutor{}
	if err := RunFlow(context.Background(), f, ex); err != nil {
		t.Fatalf("RunFlow: %v", err)
	}
	got := ex.actions()
	if len(got) != 3 {
		t.Fatalf("execute calls = %d, want 3", len(got))
	}
	for i := range f.Steps {
		if got[i].Type != f.Steps[i].Action.Type {
			t.Fatalf("call %d type = %q, want %q", i, got[i].Type, f.Steps[i].Action.Type)
		}
	}
}

// TestRunFlowAbortsOnError: the first Execute error stops the run, and the
// step context (label) is in the returned error.
func TestRunFlowAbortsOnError(t *testing.T) {
	f := &FlowScript{Name: "abort", AppVersion: "1", Steps: []FlowStep{
		{Action: Action{Type: ActionTap}, Label: "first"},
		{Action: Action{Type: ActionTap}, Label: "second"},
		{Action: Action{Type: ActionTap}, Label: "third"},
	}}
	ex := &recExecutor{failAt: 2}
	err := RunFlow(context.Background(), f, ex)
	if err == nil {
		t.Fatal("expected abort error")
	}
	if !strings.Contains(err.Error(), "second") || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("err = %v, want step label and executor error", err)
	}
	if got := ex.actions(); len(got) != 2 {
		t.Fatalf("execute calls = %d, want 2 (aborted after second step)", len(got))
	}
}

// TestRunFlowValidates: an invalid flow is rejected before any execution.
func TestRunFlowValidates(t *testing.T) {
	f := &FlowScript{Name: "bad", AppVersion: "1", Steps: []FlowStep{
		{Action: Action{Type: "fly"}},
	}}
	ex := &recExecutor{}
	if err := RunFlow(context.Background(), f, ex); err == nil {
		t.Fatal("expected validation error")
	}
	if got := ex.actions(); len(got) != 0 {
		t.Fatalf("execute calls = %d, want 0", len(got))
	}

	good := &FlowScript{Name: "good", AppVersion: "1", Steps: []FlowStep{{Action: Action{Type: ActionTap}}}}
	if err := RunFlow(context.Background(), good, nil); err == nil {
		t.Fatal("nil executor must error")
	}
}

// TestRunFlowCancellation: a cancelled context aborts between steps.
func TestRunFlowCancellation(t *testing.T) {
	f := &FlowScript{Name: "cancel", AppVersion: "1", Steps: []FlowStep{
		{Action: Action{Type: ActionTap}},
		{Action: Action{Type: ActionTap}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ex := &recExecutor{}
	if err := RunFlow(ctx, f, ex); err == nil {
		t.Fatal("expected cancellation error")
	}
	if got := ex.actions(); len(got) != 0 {
		t.Fatalf("execute calls = %d, want 0 (cancel checked before first step)", len(got))
	}
}

// TestLoadPlaceholderFlow: the delivered adapt/flows placeholder must load
// and validate; the flow loader tests reference it.
func TestLoadPlaceholderFlow(t *testing.T) {
	path := filepath.Join("..", "..", "adapt", "flows", "douyin.placeholder.json")
	f, err := LoadFlow(path)
	if err != nil {
		t.Fatalf("LoadFlow(%s): %v", path, err)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("placeholder flow must validate: %v", err)
	}
	if f.Name != "douyin-comments-open" || f.Platform != "douyin" || f.AppVersion != "0.0.0-placeholder" {
		t.Fatalf("placeholder header = %+v", f)
	}
	if len(f.Steps) != 1 {
		t.Fatalf("placeholder steps = %d, want 1", len(f.Steps))
	}
	first := f.Steps[0]
	if first.Action.Type != ActionUIAction || first.Action.Args["node_hint"] != "评论" || first.Label != "open-comments" {
		t.Fatalf("placeholder step = %+v", first)
	}

	// runs cleanly through a recorder executor
	ex := &recExecutor{}
	if err := RunFlow(context.Background(), f, ex); err != nil {
		t.Fatalf("RunFlow(placeholder): %v", err)
	}
	if got := ex.actions(); len(got) != 1 || got[0].Type != ActionUIAction {
		t.Fatalf("placeholder execution = %+v", got)
	}
}
