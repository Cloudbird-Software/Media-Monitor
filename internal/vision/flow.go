package vision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
)

// StepLog is one recorded, verdict-carrying execution step feeding Distill.
type StepLog struct {
	Action    Action `json:"action"`
	Observed  string `json:"observed,omitempty"`
	Succeeded bool   `json:"succeeded"`
}

// FlowStep is one replayable UI interaction of a FlowScript.
type FlowStep struct {
	Action Action `json:"action"`
	Expect string `json:"expect,omitempty"`
	Label  string `json:"label,omitempty"`
}

// FlowScript is a distilled, replayable interaction script pinned to a
// platform/app version.
type FlowScript struct {
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	AppVersion string     `json:"app_version"`
	Steps      []FlowStep `json:"steps"`
}

// LoadFlow reads and unmarshals one FlowScript JSON file.
func LoadFlow(path string) (*FlowScript, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("vision: load flow: %w", err)
	}
	var f FlowScript
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("vision: flow %s: %w", path, err)
	}
	return &f, nil
}

// Validate checks structural invariants: non-empty name and app version, at
// least one step, and every step action inside the action vocabulary.
func (f *FlowScript) Validate() error {
	if f == nil {
		return errors.New("vision: nil flow script")
	}
	if strings.TrimSpace(f.Name) == "" {
		return errors.New("vision: flow name is empty")
	}
	if strings.TrimSpace(f.AppVersion) == "" {
		return fmt.Errorf("vision: flow %s: app_version is empty", f.Name)
	}
	if len(f.Steps) == 0 {
		return fmt.Errorf("vision: flow %s: no steps", f.Name)
	}
	for i, s := range f.Steps {
		if !validActionType(s.Action.Type) {
			return fmt.Errorf("vision: flow %s step %d: unsupported action type %q", f.Name, i, s.Action.Type)
		}
	}
	return nil
}

// Distill compresses an execution log into a FlowScript: only succeeded
// steps are kept, consecutive steps with identical action type and args are
// deduplicated (keeping the first), and each kept step's observed string is
// stored as its Expect, truncated to 40 characters.
func Distill(name, platform, appVersion string, log []StepLog) *FlowScript {
	f := &FlowScript{Name: name, Platform: platform, AppVersion: appVersion}
	var prev Action
	havePrev := false
	for _, s := range log {
		if !s.Succeeded {
			continue
		}
		a := cloneAction(s.Action)
		if havePrev && actionsEqual(prev, a) {
			continue // consecutive duplicate; keep the first occurrence
		}
		step := FlowStep{Action: a}
		if s.Observed != "" {
			step.Expect = truncateRunes(s.Observed, 40)
		}
		f.Steps = append(f.Steps, step)
		prev = a
		havePrev = true
	}
	return f
}

// cloneAction defensively copies Args so later callers cannot mutate the
// flow's steps through the log's maps.
func cloneAction(a Action) Action {
	if len(a.Args) == 0 {
		a.Args = nil
		return a
	}
	m := make(map[string]any, len(a.Args))
	for k, v := range a.Args {
		m[k] = v
	}
	a.Args = m
	return a
}

// actionsEqual compares type and args; nil and empty arg maps are equal.
func actionsEqual(a, b Action) bool {
	// manually comparing args first avoids cheap Type asymmetry; Type is the
	// cheaper discriminator so it runs first anyway
	if a.Type != b.Type {
		return false
	}
	return argsEqual(a.Args, b.Args)
}

func argsEqual(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// truncateRunes truncates s to at most max runes (nil-safe for max < 0).
func truncateRunes(s string, max int) string {
	if max < 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// RunFlow replays the script in order through ex. The flow is validated
// first; the first Execute error aborts the run and is returned with step
// context. ctx (may be nil) is checked between steps for cancellation.
func RunFlow(ctx context.Context, f *FlowScript, ex Executor) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if ex == nil {
		return errors.New("vision: nil flow executor")
	}
	for i, s := range f.Steps {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("vision: flow %s: %w", f.Name, err)
			}
		}
		label := s.Label
		if label == "" {
			label = fmt.Sprintf("%s#%d", s.Action.Type, i)
		}
		if _, err := ex.Execute(s.Action); err != nil {
			return fmt.Errorf("vision: flow %s step %d (%s): %w", f.Name, i, label, err)
		}
	}
	return nil
}
