// visioncmd — the vision layer's CLI surface (IR-MM-0001 AC-14 / ADR-0100):
// an OpenAI-compatible provider (MEDIAMON_VISION_ENDPOINT) drives a real
// device through the adb semantic-action bridge (tap / swipe / type_text /
// screencap / uidump). The endpoint is owner-provided (ENV-REQ-3); when it
// is not configured the command fails closed with an explicit error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adb"
	"github.com/Cloudbird-Software/Media-Monitor/internal/vision"
)

// visionEnv is the provider endpoint environment variable (ADR-0100).
const visionEnv = "MEDIAMON_VISION_ENDPOINT"

// visionProviderFromEnv builds the provider from MEDIAMON_VISION_ENDPOINT
// (+ optional MEDIAMON_VISION_API_KEY / MEDIAMON_VISION_MODEL). Unset
// endpoint = explicit fail-closed error, never a silent skip.
func visionProviderFromEnv() (vision.Provider, error) {
	modelEnv := os.Getenv("MEDIAMON_VISION_MODEL")
	if modelEnv == "" {
		modelEnv = "ui-tars"
	}
	endpoint := strings.TrimSpace(os.Getenv(visionEnv))
	if endpoint == "" {
		return nil, fmt.Errorf("vision: %s not set — the vision provider requires an OpenAI-compatible endpoint (ENV-REQ-3); refusing to run", visionEnv)
	}
	return vision.NewOpenAICompat(vision.OpenAICompat{
		Endpoint: endpoint,
		APIKey:   os.Getenv("MEDIAMON_VISION_API_KEY"),
		Model:    modelEnv,
	}), nil
}

// deviceOps is the adb surface the executor needs; *adbClientOps satisfies
// it by binding an adb.Client to one serial.
type deviceOps interface {
	Tap(x, y int32) error
	Swipe(x0, y0, x1, y1 int32, ms int) error
	KeyText(text string) error
	ScreencapPNG() ([]byte, error)
	Shell(cmd string) (string, error)
	UIDump() (*adb.NodeTree, error)
}

// adbClientOps binds an adb client to a device serial.
type adbClientOps struct {
	c      *adb.Client
	serial string
}

func (d adbClientOps) Tap(x, y int32) error { return d.c.Tap(d.serial, x, y) }
func (d adbClientOps) Swipe(x0, y0, x1, y1 int32, ms int) error {
	return d.c.Swipe(d.serial, x0, y0, x1, y1, ms)
}
func (d adbClientOps) KeyText(t string) error        { return d.c.KeyText(d.serial, t) }
func (d adbClientOps) ScreencapPNG() ([]byte, error) { return d.c.ScreencapPNG(d.serial) }
func (d adbClientOps) Shell(cmd string) (string, error) {
	return d.c.Shell(d.serial, cmd)
}
func (d adbClientOps) UIDump() (*adb.NodeTree, error) { return d.c.UIDump(d.serial) }

// adbExecutor adapts deviceOps to vision.Executor: each semantic action
// maps onto exactly one adb primitive (tap / swipe / type_text / key /
// ui_action-via-uidump; screencap powers the observation loop).
type adbExecutor struct {
	dev deviceOps
}

func (e adbExecutor) Exec(cmd string, args ...string) (string, error) {
	return e.dev.Shell(strings.TrimSpace(cmd + " " + strings.Join(args, " ")))
}

func (e adbExecutor) Screenshot() ([]byte, error) { return e.dev.ScreencapPNG() }

func (e adbExecutor) Execute(a vision.Action) (string, error) {
	switch a.Type {
	case vision.ActionTap:
		x, y, err := xyFromArgs(a.Args)
		if err != nil {
			return "", err
		}
		return "tap", e.dev.Tap(x, y)
	case vision.ActionSwipe:
		get := func(k string) (int32, error) {
			f, ok := numArg(a.Args, k)
			if !ok {
				return 0, fmt.Errorf("swipe: missing arg %q", k)
			}
			return int32(f), nil
		}
		x0, e0 := get("x0")
		y0, e1 := get("y0")
		x1, e2 := get("x1")
		y1, e3 := get("y1")
		for _, err := range []error{e0, e1, e2, e3} {
			if err != nil {
				return "", err
			}
		}
		ms := 300
		if f, ok := numArg(a.Args, "duration_ms"); ok {
			ms = int(f)
		} else if f, ok := numArg(a.Args, "ms"); ok {
			ms = int(f) // the provider prompt's alias
		}
		return "swipe", e.dev.Swipe(x0, y0, x1, y1, ms)
	case vision.ActionTypeText:
		s, _ := a.Args["text"].(string)
		if s == "" {
			return "", errors.New("type_text: missing text arg")
		}
		return "type_text", e.dev.KeyText(s)
	case vision.ActionKey:
		s, _ := a.Args["key"].(string)
		if s == "" {
			return "", errors.New("key: missing key arg")
		}
		out, err := e.dev.Shell("input keyevent " + s)
		return out, err
	case vision.ActionUIAction:
		// Accept both "hint" and the provider prompt's "node_hint" key so a
		// schema-faithful endpoint drives the bridge without a translation
		// layer (holdout H6 finding).
		hint, _ := a.Args["hint"].(string)
		if hint == "" {
			hint, _ = a.Args["node_hint"].(string)
		}
		if hint == "" {
			return "", errors.New("ui_action: missing hint/node_hint arg")
		}
		tree, err := e.dev.UIDump()
		if err != nil {
			return "", err
		}
		node := findNodeByHint(tree, hint)
		if node == nil {
			return "", fmt.Errorf("ui_action: no node matching hint %q", hint)
		}
		rect, err := adb.ParseBounds(node.Bounds)
		if err != nil {
			return "", fmt.Errorf("ui_action: node bounds %q: %w", node.Bounds, err)
		}
		cx, cy := rect.Center()
		return "ui_action:" + hint, e.dev.Tap(int32(cx), int32(cy))

	case vision.ActionDone:
		return "done", nil
	default:
		return "", fmt.Errorf("vision executor: unsupported action %q", a.Type)
	}
}

// findNodeByHint scans the dump tree for a node whose text, resource id or
// class contains the hint (case-insensitive).
func findNodeByHint(tree *adb.NodeTree, hint string) *adb.Node {
	if tree == nil || tree.Root == nil {
		return nil
	}
	h := strings.ToLower(hint)
	var walk func(n *adb.Node) *adb.Node
	walk = func(n *adb.Node) *adb.Node {
		if strings.Contains(strings.ToLower(n.Text), h) ||
			strings.Contains(strings.ToLower(n.ResourceID), h) ||
			strings.Contains(strings.ToLower(n.Class), h) {
			return n
		}
		for _, c := range n.Children {
			if found := walk(c); found != nil {
				return found
			}
		}
		return nil
	}
	return walk(tree.Root)
}

func numArg(args map[string]any, k string) (float64, bool) {
	switch v := args[k].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

func xyFromArgs(args map[string]any) (int32, int32, error) {
	x, okx := numArg(args, "x")
	y, oky := numArg(args, "y")
	if !okx || !oky {
		return 0, 0, errors.New("tap: missing x/y args")
	}
	return int32(x), int32(y), nil
}

// visionRun drives the observe-act loop: screencap → provider.Act →
// executor.Execute, until the provider reports done or steps run out. On
// success the turn log distills into a replayable flow script (existing
// mechanism).
func visionRun(ctx context.Context, p vision.Provider, ex vision.Executor, goal string, maxSteps int, distillPath string) ([]vision.Turn, error) {
	if maxSteps <= 0 {
		maxSteps = 15
	}
	var history []vision.Turn
	var log []vision.StepLog
	for step := 0; step < maxSteps; step++ {
		screen, err := ex.Screenshot()
		if err != nil {
			return history, fmt.Errorf("vision: screencap: %w", err)
		}
		act, note, err := p.Act(ctx, screen, goal, history)
		if err != nil {
			return history, fmt.Errorf("vision: provider: %w", err)
		}
		out, err := ex.Execute(act)
		turn := vision.Turn{Goal: goal, Action: act, Observed: note}
		history = append(history, turn)
		log = append(log, vision.StepLog{Action: act, Observed: out, Succeeded: err == nil})
		if err != nil {
			return history, fmt.Errorf("vision: execute %s: %w", act.Type, err)
		}
		if act.Type == vision.ActionDone {
			if distillPath != "" {
				flow := vision.Distill(goal, "android", "vision-run", log)
				b, merr := json.MarshalIndent(flow, "", "  ")
				if merr == nil {
					_ = os.WriteFile(distillPath, b, 0o644)
				}
			}
			return history, nil
		}
	}
	return history, fmt.Errorf("vision: goal not reached within %d steps", maxSteps)
}

func cmdVision(args []string) error {
	fs := flag.NewFlagSet("vision", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "use: vision run (see mediactl help)\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("missing vision subcommand")
	}
	switch fs.Arg(0) {
	case "run":
		return visionRunCmd(fs.Args()[1:])
	default:
		return fmt.Errorf("unknown vision subcommand %q", fs.Arg(0))
	}
}

func visionRunCmd(args []string) error {
	fs := flag.NewFlagSet("vision run", flag.ExitOnError)
	goal := fs.String("goal", "", "natural-language goal for the vision provider (required)")
	serial := fs.String("serial", "", "device serial (required)")
	maxSteps := fs.Int("max-steps", 15, "observe-act loop budget")
	distill := fs.String("distill", "", "write the distilled flow script to this path on success")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *goal == "" || *serial == "" {
		return fmt.Errorf("--goal and --serial are required")
	}
	p, err := visionProviderFromEnv()
	if err != nil {
		return err
	}
	client, err := adb.Connect(visionADBAddr())
	if err != nil {
		return err
	}
	defer client.Close()
	ex := adbExecutor{dev: adbClientOps{c: client, serial: *serial}}
	turns, err := visionRun(context.Background(), p, ex, *goal, *maxSteps, *distill)
	for _, t := range turns {
		fmt.Printf("{\"action\":%q,\"observed\":%q}\n", t.Action.Type, t.Observed)
	}
	return err
}

// visionADBAddr resolves the adb server address (MEDIAMON_ADB_ADDR override,
// default 127.0.0.1:5037 — same convention as mediad-mcp).
func visionADBAddr() string {
	if a := os.Getenv("MEDIAMON_ADB_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:5037"
}
