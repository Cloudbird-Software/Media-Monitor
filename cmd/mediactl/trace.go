package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adb"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
	"github.com/Cloudbird-Software/Media-Monitor/internal/tasks"
	"github.com/Cloudbird-Software/Media-Monitor/internal/trace"
)

// cmdTrace dispatches trace subcommands.
func cmdTrace(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use: trace run --platform <p> --targets <sec_uid,...> [--flow <file>] [--adb <addr>] [...]")
	}
	switch args[0] {
	case "run":
		return traceRun(args[1:])
	default:
		return fmt.Errorf("unknown trace subcommand %q", args[0])
	}
}

func traceRun(args []string) error {
	fs := flag.NewFlagSet("trace run", flag.ExitOnError)
	platform := fs.String("platform", "", "platform: douyin|kuaishou|xhs")
	targets := fs.String("targets", "", "comma-separated target sec_uids")
	flowFile := fs.String("flow", "", "flow JSON file (default: adapt/flows/<platform>-trace.json)")
	adbAddr := fs.String("adb", "127.0.0.1:5037", "adb server address")
	account := fs.String("account", "", "account id to act as for DM (empty = platform default)")
	first := fs.String("dm-first", "", "DM first message (enables dm action)")
	second := fs.String("dm-second", "", "DM second message")
	delay := fs.Int64("dm-delay-ms", 15000, "delay before DM second message")
	signerURL := fs.String("signer-url", os.Getenv("MEDIAMON_SIGNER_URL"), "signer base URL")
	cookieFile := fs.String("cookies", "", "cookie file (for DM)")
	dataDir := fs.String("data", filepath.Join("data", "trace"), "store dir for DM send counters")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *platform == "" || *targets == "" {
		return fmt.Errorf("--platform and --targets are required")
	}
	if err := requireLicense("dm"); err != nil {
		return err
	}

	flow, err := loadTraceFlow(*flowFile, *platform)
	if err != nil {
		return err
	}
	// The profile deep link is flow-declared (profile_url_template); the adb
	// executor fails closed without it, so reject here before touching adb.
	if flow.ProfileURLTemplate == "" {
		return fmt.Errorf("trace: flow for platform %q declares no profile_url_template (fail-closed: refusing to guess a platform deep link)", *platform)
	}
	devs, err := trace.DiscoverDevices(*adbAddr)
	if err != nil {
		return fmt.Errorf("discover devices: %w", err)
	}
	if len(devs) == 0 {
		return fmt.Errorf("no adb devices found at %s", *adbAddr)
	}
	tgts := make([]trace.Target, 0, len(splitAndTrim(*targets, ",")))
	for _, s := range splitAndTrim(*targets, ",") {
		tgts = append(tgts, trace.Target{SecUID: s})
	}

	var opts []trace.RunnerOption
	// Gesture executor with an empty layout: declared actions fire, and any
	// action whose bounds are not in the layout is treated as a non-fatal skip
	// (the CLI uses the real adb client; filling the layout from
	// vision-distilled bounds is future work). The flow's profile_url_template
	// is injected so Prepare can open the target's home page.
	gesture, adbConn, err := traceGestureExecutor(*adbAddr, flow)
	if err != nil {
		return err
	}
	defer adbConn.Close()
	opts = append(opts, trace.WithGestureExecutor(gesture))

	if *first != "" {
		eng, err := buildSendEngine(*platform, *cookieFile, *signerURL, *account)
		if err != nil {
			return err
		}
		st, err := store.Open(*dataDir)
		if err != nil {
			return err
		}
		defer st.Close()
		dm := trace.NewDMExecutor(tasks.NewSender(eng, st), *first, second, *delay)
		opts = append(opts, trace.WithDMExecutor(dm))
	}

	runner, err := trace.NewRunner(flow, devs, opts...)
	if err != nil {
		return err
	}
	rep, err := runner.Run(context.Background(), tgts, nil)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func loadTraceFlow(flowFile, platform string) (trace.Flow, error) {
	if flowFile != "" {
		return trace.LoadFlow(flowFile)
	}
	return trace.LoadFlowFor(adaptDir(), platform)
}

// traceGestureExecutor builds the adb gesture executor for a flow: it
// connects to the adb server and injects the flow-declared profile URL
// template (fail-closed — an empty template never reaches the executor).
// The returned client must be closed by the caller.
func traceGestureExecutor(adbAddr string, flow trace.Flow) (*trace.AdbExecutor, *adb.Client, error) {
	if flow.ProfileURLTemplate == "" {
		return nil, nil, fmt.Errorf("trace: flow %q declares no profile_url_template (fail-closed)", flow.Platform)
	}
	conn, err := adb.Connect(adbAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("connect adb server %s: %w", adbAddr, err)
	}
	return trace.NewAdbExecutor(conn, trace.Layout{}, trace.WithProfileURLTemplate(flow.ProfileURLTemplate)), conn, nil
}
