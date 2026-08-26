package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adb"
	"github.com/Cloudbird-Software/Media-Monitor/internal/tasks"
)

// DMExecutor sends the DM action of a trace flow via tasks.Sender (M2),
// reusing its send-cap bookkeeping and retry semantics.
type DMExecutor struct {
	sender *tasks.Sender
	first  string
	second *tasks.MessageTemplate
	delay  int64
}

// NewDMExecutor builds a DM executor. first is required; second/delay mirror
// the M2 two-message flow.
func NewDMExecutor(sender *tasks.Sender, first string, second *string, delayMs int64) *DMExecutor {
	dm := &DMExecutor{sender: sender, first: first, delay: delayMs}
	if second != nil {
		dm.second = &tasks.MessageTemplate{Content: *second}
	}
	return dm
}

// Send runs the DM flow for one target.
func (d *DMExecutor) Send(ctx context.Context, platform, secUID, nickname string) error {
	cfg := tasks.SendTaskConfig{
		Platform:       platform,
		Targets:        []string{secUID},
		FirstMessage:   tasks.MessageTemplate{Content: d.first},
		SecondDelayMs:  d.delay,
		SubstituteNick: map[string]string{secUID: nickname},
	}
	if d.second != nil {
		cfg.SecondMessage = d.second
	}
	_, err := d.sender.Run(ctx, cfg)
	return err
}

// Runner ties together a flow, a device source, the gesture executor and the
// DM executor to run a complete trace job.
type Runner struct {
	scheduler *Scheduler
	flow      Flow
	devices   []Device
	gesture   Executor
	dm        *DMExecutor
	platform  string
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithGestureExecutor sets the gesture executor (default: nil, must be set if
// the flow contains non-DM actions).
func WithGestureExecutor(e Executor) RunnerOption { return func(r *Runner) { r.gesture = e } }

// WithDMExecutor sets the DM executor for dm-typed actions.
func WithDMExecutor(dm *DMExecutor) RunnerOption { return func(r *Runner) { r.dm = dm } }

// NewRunner builds a Runner for a platform. devices must be non-empty.
func NewRunner(flow Flow, devices []Device, opts ...RunnerOption) (*Runner, error) {
	if len(devices) == 0 {
		return nil, errors.New("trace: at least one device is required")
	}
	r := &Runner{
		scheduler: NewScheduler(),
		flow:      flow,
		devices:   devices,
		platform:  flow.Platform,
	}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// Run executes the flow over targets. dmTargets supply per-target nicknames
// for {nickname} substitution.
func (r *Runner) Run(ctx context.Context, targets []Target, dmTargets map[string]string) (*Report, error) {
	// Composite executor: gesture actions go to the gesture executor, dm
	// actions go to the DM executor. If an action type has no executor it is
	// treated as a non-fatal skip.
	comp := &compositeExecutor{gesture: r.gesture, dm: r.dm, dmTargets: dmTargets, platform: r.platform}
	return r.scheduler.Run(ctx, r.flow, targets, r.devices, comp)
}

// compositeExecutor routes each action to the right executor.
type compositeExecutor struct {
	gesture   Executor
	dm        *DMExecutor
	dmTargets map[string]string
	platform  string
}

func (c *compositeExecutor) Prepare(ctx context.Context, dev Device, t Target) error {
	if c.gesture != nil {
		return c.gesture.Prepare(ctx, dev, t)
	}
	return nil
}

func (c *compositeExecutor) Run(ctx context.Context, dev Device, t Target, a Action) (int64, bool, error) {
	switch a.Type {
	case ActionDM:
		if c.dm == nil {
			return 0, true, nil
		}
		nick := t.Nickname
		if nick == "" {
			nick = c.dmTargets[t.SecUID]
		}
		if err := c.dm.Send(ctx, c.platform, t.SecUID, nick); err != nil {
			return 0, false, err
		}
		return 0, false, nil
	default:
		if c.gesture == nil {
			return 0, true, nil
		}
		return c.gesture.Run(ctx, dev, t, a)
	}
}

func (c *compositeExecutor) Release(ctx context.Context, dev Device, t Target) error {
	if c.gesture != nil {
		return c.gesture.Release(ctx, dev, t)
	}
	return nil
}

// LoadFlow reads a platform flow JSON (adapt/flows/<platform>-trace.json).
func LoadFlow(path string) (Flow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Flow{}, fmt.Errorf("trace: read flow %s: %w", path, err)
	}
	var f Flow
	if err := json.Unmarshal(data, &f); err != nil {
		return Flow{}, fmt.Errorf("trace: parse flow %s: %w", path, err)
	}
	return f, nil
}

// LoadFlowFor loads the flow for a platform from the adapt/flows dir.
func LoadFlowFor(adaptDir, platform string) (Flow, error) {
	return LoadFlow(filepath.Join(adaptDir, "flows", platform+"-trace.json"))
}

// DiscoverDevices lists adb devices via the given server address.
func DiscoverDevices(serverAddr string) ([]Device, error) {
	var c adb.Client
	serials, err := c.ListDevices(serverAddr)
	if err != nil {
		return nil, err
	}
	devs := make([]Device, 0, len(serials))
	for _, s := range serials {
		devs = append(devs, Device{Serial: s})
	}
	return devs, nil
}
