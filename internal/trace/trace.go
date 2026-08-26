// Package trace is the platform-independent "留痕" (trace/engagement) engine:
// given a queue of target users, it executes a probabilistic sequence of
// engagement actions (like/follow/collect/DM/comment, home-page dwell, work
// browsing) distributed evenly across a pool of devices. Platform differences
// (which actions exist, their default probabilities, the adb gesture
// coordinates) are declared in adapt/flows/<platform>-trace.json; this package
// owns only the scheduling policy: probability roll, duration randomization,
// target→device equalization, and the executor abstraction.
//
// The adb executor (the built-in gesture executor) performs each
// gesture by calling internal/adb tap/swipe/text and locating UI elements via
// uiautomator dumps. DM-type actions reuse tasks.Sender from M2.
package trace

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ActionType is one engagement gesture.
type ActionType string

const (
	ActionLike       ActionType = "like"
	ActionAvatarLike ActionType = "avatar_like"
	ActionFollow     ActionType = "follow"
	ActionCollect    ActionType = "collect"
	ActionDM         ActionType = "dm"
	ActionComment    ActionType = "comment"
	ActionDwell      ActionType = "dwell"  // home-page stay
	ActionBrowse     ActionType = "browse" // work browsing
)

// Action is one step in a trace sequence.
type Action struct {
	Type       ActionType `json:"type"`
	Prob       float64    `json:"prob"`                  // 0..1; 0 = never, 1 = always
	DurationMs [2]int64   `json:"duration_ms,omitempty"` // [min,max] for dwell/browse
}

// Flow is the declared trace sequence + default probabilities for one
// platform (mirrors adapt/flows/<platform>-trace.json).
type Flow struct {
	Platform string `json:"platform"`
	// ProfileURLTemplate is the per-platform profile deep-link template used
	// by gesture executors to open a target's home page; it must contain the
	// {sec_uid} placeholder. Declared in the flow JSON so the core never
	// hardcodes a platform scheme; a missing template fails closed.
	ProfileURLTemplate string   `json:"profile_url_template,omitempty"`
	Actions            []Action `json:"actions"`
}

// RenderProfileURL renders the flow's profile URL template for a target.
// Fail-closed: an empty template or one without the {sec_uid} placeholder is
// an error (the core must never invent a platform deep link).
func (f Flow) RenderProfileURL(secUID string) (string, error) {
	if f.ProfileURLTemplate == "" {
		return "", errors.New("trace: flow has no profile_url_template (refusing to guess a platform deep link)")
	}
	if !strings.Contains(f.ProfileURLTemplate, "{sec_uid}") {
		return "", fmt.Errorf("trace: profile_url_template %q lacks the {sec_uid} placeholder", f.ProfileURLTemplate)
	}
	return strings.ReplaceAll(f.ProfileURLTemplate, "{sec_uid}", secUID), nil
}

// Target is one user to engage.
type Target struct {
	SecUID   string         `json:"sec_uid"`
	Nickname string         `json:"nickname,omitempty"`
	Payload  map[string]any `json:"payload,omitempty"`
}

// Device is one adb device available for trace work.
type Device struct {
	Serial string `json:"serial"`
}

// Executor performs one concrete action on one device. Implementations are
// platform/gesture specific (adb gestures, DM via tasks.Sender, ...).
type Executor interface {
	// Prepare is called once before a target's action sequence (e.g. navigate
	// to the target's profile). errNonFatal lets the scheduler continue with
	// the next action when a gesture is not applicable.
	Prepare(ctx context.Context, dev Device, t Target) error
	// Run executes one action on the device for the given target. Returns
	// errNonFatal=true when the action could not be performed but the rest of
	// the sequence should continue.
	Run(ctx context.Context, dev Device, t Target, a Action) (durationMs int64, errNonFatal bool, err error)
	// Release is called once after a target's sequence (e.g. return home).
	Release(ctx context.Context, dev Device, t Target) error
}

// Result is the outcome for one target on one device.
type Result struct {
	Target  string         `json:"target"`
	Device  string         `json:"device"`
	Actions []ActionResult `json:"actions"`
	Skipped int            `json:"skipped"`
	Error   string         `json:"error,omitempty"`
}

// ActionResult records whether one action fired.
type ActionResult struct {
	Action     ActionType `json:"action"`
	Performed  bool       `json:"performed"`
	DurationMs int64      `json:"duration_ms,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Report aggregates one trace job.
type Report struct {
	Platform string   `json:"platform"`
	Results  []Result `json:"results"`
}

// Scheduler drives a trace job: assign targets to devices evenly, roll
// probability per action, randomize durations, and wait out dwell/browse
// stays through an injectable Sleep (default time.Sleep).
type Scheduler struct {
	mu  sync.Mutex
	rng *rand.Rand
	// Sleep waits out one dwell/browse stay. Injectable for tests (a fake
	// clock records the wait sequence; no real sleeping in tests).
	Sleep func(d time.Duration)
}

// NewScheduler builds a Scheduler with a time-seeded RNG and the real clock.
func NewScheduler() *Scheduler {
	return &Scheduler{rng: rand.New(rand.NewSource(time.Now().UnixNano())), Sleep: time.Sleep}
}

// Run executes the flow over targets using devices and the executor. With
// N devices the i-th target goes to device i mod N (equalization). Each
// action fires when a uniform roll is strictly below its Prob (Prob 0 never
// fires, Prob 1 always fires). Dwell/browse durations are uniform-random in
// their [min,max] interval; after the gesture fires the scheduler actually
// waits out the duration via Sleep (the original software stays on the page
// for the randomized interval).
func (s *Scheduler) Run(ctx context.Context, flow Flow, targets []Target, devices []Device, exec Executor) (*Report, error) {
	if len(targets) == 0 {
		return nil, errors.New("trace: no targets")
	}
	if len(devices) == 0 {
		return nil, errors.New("trace: no devices")
	}
	if exec == nil {
		return nil, errors.New("trace: executor is required")
	}
	rep := &Report{Platform: flow.Platform}
	for i, t := range targets {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		dev := devices[i%len(devices)]
		res := Result{Target: t.SecUID, Device: dev.Serial}
		if err := exec.Prepare(ctx, dev, t); err != nil {
			res.Error = err.Error()
			rep.Results = append(rep.Results, res)
			continue
		}
		for _, a := range flow.Actions {
			if err := ctx.Err(); err != nil {
				rep.Results = append(rep.Results, res)
				return rep, err
			}
			performed, dur, skipped, fatal, aerr := s.execOne(ctx, dev, t, a, exec)
			ar := ActionResult{Action: a.Type, Performed: performed, DurationMs: dur}
			if aerr != nil {
				ar.Error = aerr.Error()
			}
			if skipped {
				res.Skipped++
			}
			res.Actions = append(res.Actions, ar)
			if fatal {
				// A fatal gesture error (e.g. device gone) aborts the whole
				// job; a non-fatal error was recorded above and the loop
				// continues.
				rep.Results = append(rep.Results, res)
				return rep, aerr
			}
		}
		if err := exec.Release(ctx, dev, t); err != nil && res.Error == "" {
			res.Error = err.Error()
		}
		rep.Results = append(rep.Results, res)
	}
	return rep, nil
}

func (s *Scheduler) execOne(ctx context.Context, dev Device, t Target, a Action, exec Executor) (performed bool, dur int64, skipped bool, fatal bool, err error) {
	s.mu.Lock()
	roll := s.rng.Float64()
	s.mu.Unlock()
	if roll >= a.Prob {
		return false, 0, true, false, nil // probability gate: not fired
	}
	switch a.Type {
	case ActionDwell, ActionBrowse:
		dur = s.randomDuration(a.DurationMs)
	}
	ms, nonFatal, e := exec.Run(ctx, dev, t, a)
	if e != nil && nonFatal {
		// performed-with-error, continue the sequence.
		return true, ms, false, false, e
	}
	if e != nil {
		// fatal: abort the job.
		return false, 0, false, true, e
	}
	if dur > 0 {
		// The stay is real: wait out the randomized dwell/browse duration
		// (injectable clock; tests observe the waits without real sleeping).
		if serr := s.sleep(ctx, dur); serr != nil {
			return false, 0, false, true, serr
		}
		ms = dur
	}
	return true, ms, false, false, nil
}

// sleep waits ms milliseconds via the injected Sleep, honoring context
// cancellation before and after the wait.
func (s *Scheduler) sleep(ctx context.Context, ms int64) error {
	if ms <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sleep := s.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	sleep(time.Duration(ms) * time.Millisecond)
	return ctx.Err()
}

func (s *Scheduler) randomDuration(d [2]int64) int64 {
	if d[0] >= d[1] {
		return d[0]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return d[0] + s.rng.Int63n(d[1]-d[0]+1)
}

// Equalize returns the device assignment index for each target (i mod N) —
// exported for tests/inspection.
func Equalize(targetCount, deviceCount int) []int {
	out := make([]int, targetCount)
	for i := range out {
		out[i] = i % deviceCount
	}
	return out
}

// SortTargetsByDevice returns targets grouped by their assigned device (stable,
// deterministic given the equalization rule).
func SortTargetsByDevice(targets []Target, devices []Device) [][]Target {
	groups := make([][]Target, len(devices))
	for i, t := range targets {
		groups[i%len(devices)] = append(groups[i%len(devices)], t)
	}
	return groups
}
