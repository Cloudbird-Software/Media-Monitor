package trace

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeExecutor records every Run call so tests can assert the action sequence
// and the probability/duration decisions the Scheduler made.
type fakeExecutor struct {
	mu      sync.Mutex
	prepare int
	release int
	runs    []ActionType
	// failOn, if set, makes Run return a fatal error for that action type.
	failOn ActionType
	// nonFatalOn, if set, makes Run return a non-fatal error for that action.
	nonFatalOn ActionType
}

func (f *fakeExecutor) Prepare(_ context.Context, _ Device, _ Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepare++
	return nil
}

func (f *fakeExecutor) Run(_ context.Context, _ Device, t Target, a Action) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, a.Type)
	if a.Type == f.failOn {
		return 0, false, errors.New("fatal: " + string(a.Type))
	}
	if a.Type == f.nonFatalOn {
		return 0, true, errors.New("nonfatal: " + string(a.Type))
	}
	return 100, false, nil
}

func (f *fakeExecutor) Release(_ context.Context, _ Device, _ Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.release++
	return nil
}

// TestProbabilityBoundaryZeroNeverFires: an action with Prob 0 must never be
// performed, regardless of RNG.
func TestProbabilityBoundaryZeroNeverFires(t *testing.T) {
	exec := &fakeExecutor{}
	s := NewScheduler()
	flow := Flow{
		Platform: "douyin",
		Actions: []Action{
			{Type: ActionLike, Prob: 0},
			{Type: ActionFollow, Prob: 0},
		},
	}
	targets := []Target{{SecUID: "t1"}}
	devices := []Device{{Serial: "d1"}}
	rep, err := s.Run(context.Background(), flow, targets, devices, exec)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("results = %d", len(rep.Results))
	}
	for _, ar := range rep.Results[0].Actions {
		if ar.Performed {
			t.Fatalf("prob=0 action %s must not fire", ar.Action)
		}
	}
	if rep.Results[0].Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", rep.Results[0].Skipped)
	}
}

// TestProbabilityBoundaryOneAlwaysFires: an action with Prob 1 must always fire.
func TestProbabilityBoundaryOneAlwaysFires(t *testing.T) {
	exec := &fakeExecutor{}
	s := NewScheduler()
	flow := Flow{
		Platform: "douyin",
		Actions: []Action{
			{Type: ActionLike, Prob: 1},
			{Type: ActionFollow, Prob: 1},
			{Type: ActionCollect, Prob: 1},
		},
	}
	targets := []Target{{SecUID: "t1"}}
	devices := []Device{{Serial: "d1"}}
	rep, err := s.Run(context.Background(), flow, targets, devices, exec)
	if err != nil {
		t.Fatal(err)
	}
	for _, ar := range rep.Results[0].Actions {
		if !ar.Performed {
			t.Fatalf("prob=1 action %s must fire", ar.Action)
		}
	}
	if rep.Results[0].Skipped != 0 {
		t.Fatalf("skipped = %d, want 0", rep.Results[0].Skipped)
	}
}

func TestEqualizationRoundRobin(t *testing.T) {
	assign := Equalize(5, 2)
	want := []int{0, 1, 0, 1, 0}
	for i, a := range assign {
		if a != want[i] {
			t.Fatalf("assign = %v, want %v", assign, want)
		}
	}
	groups := SortTargetsByDevice([]Target{{SecUID: "a"}, {SecUID: "b"}, {SecUID: "c"}}, []Device{{Serial: "d1"}, {Serial: "d2"}})
	if len(groups) != 2 || len(groups[0]) != 2 || len(groups[1]) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0][0].SecUID != "a" || groups[0][1].SecUID != "c" || groups[1][0].SecUID != "b" {
		t.Fatalf("group order wrong: %+v", groups)
	}
}

func TestSchedulerPrepareReleasePerTarget(t *testing.T) {
	exec := &fakeExecutor{}
	s := NewScheduler()
	flow := Flow{Platform: "douyin", Actions: []Action{{Type: ActionLike, Prob: 1}}}
	targets := []Target{{SecUID: "t1"}, {SecUID: "t2"}, {SecUID: "t3"}}
	devices := []Device{{Serial: "d1"}}
	_, err := s.Run(context.Background(), flow, targets, devices, exec)
	if err != nil {
		t.Fatal(err)
	}
	if exec.prepare != 3 || exec.release != 3 {
		t.Fatalf("prepare=%d release=%d, want 3/3", exec.prepare, exec.release)
	}
	if len(exec.runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(exec.runs))
	}
}

func TestSchedulerFatalErrorAbortsJob(t *testing.T) {
	exec := &fakeExecutor{failOn: ActionFollow}
	s := NewScheduler()
	flow := Flow{Platform: "douyin", Actions: []Action{
		{Type: ActionLike, Prob: 1},
		{Type: ActionFollow, Prob: 1}, // fatal here
		{Type: ActionCollect, Prob: 1},
	}}
	targets := []Target{{SecUID: "t1"}, {SecUID: "t2"}}
	devices := []Device{{Serial: "d1"}}
	rep, err := s.Run(context.Background(), flow, targets, devices, exec)
	if err == nil {
		t.Fatal("expected fatal error to abort the job")
	}
	// Only the first target is processed; it records like (performed) and
	// follow (fatal) then aborts — collect and target t2 are never reached.
	if len(rep.Results) != 1 {
		t.Fatalf("results = %d, want 1 (aborted after first target)", len(rep.Results))
	}
	acts := rep.Results[0].Actions
	if len(acts) != 2 {
		t.Fatalf("actions = %d, want 2 (like + fatal follow)", len(acts))
	}
	if !acts[0].Performed || acts[1].Performed {
		t.Fatalf("expected like performed, follow not: %+v", acts)
	}
	if acts[1].Error == "" {
		t.Fatalf("expected error on fatal action")
	}
}

func TestSchedulerNonFatalErrorContinues(t *testing.T) {
	exec := &fakeExecutor{nonFatalOn: ActionLike}
	s := NewScheduler()
	flow := Flow{Platform: "douyin", Actions: []Action{
		{Type: ActionLike, Prob: 1},
		{Type: ActionFollow, Prob: 1},
	}}
	targets := []Target{{SecUID: "t1"}}
	devices := []Device{{Serial: "d1"}}
	rep, err := s.Run(context.Background(), flow, targets, devices, exec)
	if err != nil {
		t.Fatal(err)
	}
	acts := rep.Results[0].Actions
	if !acts[0].Performed || !acts[1].Performed {
		t.Fatalf("both should be performed (non-fatal): %+v", acts)
	}
	if acts[0].Error == "" {
		t.Fatalf("expected non-fatal error recorded")
	}
}

func TestDurationRandomizedWithinRange(t *testing.T) {
	s := NewScheduler()
	for i := 0; i < 50; i++ {
		d := s.randomDuration([2]int64{100, 200})
		if d < 100 || d > 200 {
			t.Fatalf("duration %d out of [100,200]", d)
		}
	}
}

// TestDwellBrowseReallyWaits: dwell/browse stays must actually wait out the
// randomized duration through the injectable Sleep. A fake clock records the
// wait sequence; the test does zero real sleeping (durations are far larger
// than the test runtime).
func TestDwellBrowseReallyWaits(t *testing.T) {
	var mu sync.Mutex
	var waits []int64 // milliseconds recorded by the fake clock
	exec := &fakeExecutor{}
	s := NewScheduler()
	s.Sleep = func(d time.Duration) {
		mu.Lock()
		waits = append(waits, d.Milliseconds())
		mu.Unlock()
	}
	flow := Flow{
		Platform: "douyin",
		Actions: []Action{
			{Type: ActionLike, Prob: 1},
			{Type: ActionDwell, Prob: 1, DurationMs: [2]int64{3000, 8000}},
			{Type: ActionBrowse, Prob: 1, DurationMs: [2]int64{2000, 5000}},
		},
	}
	targets := []Target{{SecUID: "t1"}, {SecUID: "t2"}}
	devices := []Device{{Serial: "d1"}}
	rep, err := s.Run(context.Background(), flow, targets, devices, exec)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// 2 targets × (1 dwell + 1 browse) = 4 waits; the like action waits 0.
	if len(waits) != 4 {
		t.Fatalf("waits = %v, want 4 entries", waits)
	}
	for i, w := range waits {
		lo, hi := int64(3000), int64(8000)
		if i%2 == 1 {
			lo, hi = 2000, 5000
		}
		if w < lo || w > hi {
			t.Fatalf("wait[%d] = %dms, want within [%d,%d]", i, w, lo, hi)
		}
	}
	// The reported durations match the recorded waits.
	var reported []int64
	for _, res := range rep.Results {
		for _, ar := range res.Actions {
			if ar.Action == ActionDwell || ar.Action == ActionBrowse {
				reported = append(reported, ar.DurationMs)
			}
		}
	}
	if len(reported) != len(waits) {
		t.Fatalf("reported durations = %v, waits = %v", reported, waits)
	}
	for i := range waits {
		if reported[i] != waits[i] {
			t.Fatalf("reported[%d] = %d, waited %d", i, reported[i], waits[i])
		}
	}
}

// TestDwellWaitHonorsCancel: a cancelled context aborts the job at the wait.
func TestDwellWaitHonorsCancel(t *testing.T) {
	exec := &fakeExecutor{}
	s := NewScheduler()
	s.Sleep = func(d time.Duration) {} // fake clock: instant
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	flow := Flow{Platform: "douyin", Actions: []Action{
		{Type: ActionDwell, Prob: 1, DurationMs: [2]int64{1000, 1000}},
	}}
	_, err := s.Run(ctx, flow, []Target{{SecUID: "t1"}}, []Device{{Serial: "d1"}}, exec)
	if err == nil {
		t.Fatal("expected context cancellation to abort the job")
	}
}

// TestLoadPlatformFlows: the shipped flow JSONs parse and every one declares
// a renderable profile_url_template (the AdbExecutor fails closed without it).
func TestLoadPlatformFlows(t *testing.T) {
	flowsDir := filepath.Join("..", "..", "adapt", "flows")
	for _, name := range []string{"douyin-trace.json", "shipinhao-trace.json"} {
		f, err := LoadFlow(filepath.Join(flowsDir, name))
		if err != nil {
			t.Fatalf("LoadFlow(%s): %v", name, err)
		}
		if len(f.Actions) == 0 {
			t.Fatalf("%s: no actions", name)
		}
		if _, err := f.RenderProfileURL("MS4wLjABAAAA"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestRunRequiresTargetsAndDevices(t *testing.T) {
	s := NewScheduler()
	exec := &fakeExecutor{}
	if _, err := s.Run(context.Background(), Flow{}, nil, []Device{{Serial: "d"}}, exec); err == nil {
		t.Fatal("expected error for no targets")
	}
	if _, err := s.Run(context.Background(), Flow{}, []Target{{SecUID: "t"}}, nil, exec); err == nil {
		t.Fatal("expected error for no devices")
	}
	if _, err := s.Run(context.Background(), Flow{}, []Target{{SecUID: "t"}}, []Device{{Serial: "d"}}, nil); err == nil {
		t.Fatal("expected error for nil executor")
	}
}
