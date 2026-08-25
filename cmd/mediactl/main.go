// Command mediactl is the Media-Monitor CLI: contract listing, offline
// canary/diff of the adaptation harness, task ops, and version. Collector
// and live subcommands attach in later PRs.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adapt"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

const version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version":
		err = cmdVersion(os.Args[2:])
	case "contracts":
		err = cmdContracts(os.Args[2:])
	case "adapt":
		err = cmdAdapt(os.Args[2:])
	case "tasks":
		err = cmdTasks(os.Args[2:])
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `mediactl — media-monitor command line

usage: mediactl <command> [flags]

commands:
  version                          print version
  contracts list                   list registered platform contracts
  adapt canary --offline [name]    run adaptation canaries (fixtures only;
                                   add --live when live driver exists)
  adapt diff --contract <name> --fixture <file> [--kind kind]
                                   diff one contract against one payload
  adapt snapshot --accept <name>   placeholder: regenerate fixture from a
                                   captured payload (review diff first)
  tasks submit --kind <k> --config <json>   submit a task to the local store
  tasks list --data <dir>          list tasks from the local store
`)
}

func cmdVersion(args []string) error {
	fmt.Println("mediactl version", version)
	return nil
}

func adaptDir() string {
	if d := os.Getenv("MEDIAMON_ADAPT_DIR"); d != "" {
		return d
	}
	return "adapt"
}

func loadRegistry() (*contracts.Registry, *adapt.Runner, error) {
	dir := adaptDir()
	reg := contracts.NewRegistry()
	cdir := filepath.Join(dir, "contracts")
	if _, err := os.Stat(cdir); err != nil {
		return nil, nil, fmt.Errorf("contracts dir %s: %w", cdir, err)
	}
	if err := contracts.LoadDir(reg, cdir); err != nil {
		return nil, nil, err
	}
	return reg, adapt.NewRunner(reg, filepath.Join(dir, "fixtures"), filepath.Join(dir, "canaries")), nil
}

func cmdContracts(args []string) error {
	fs := flag.NewFlagSet("contracts", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() > 0 && fs.Arg(0) != "list" {
		return fmt.Errorf("unknown contracts subcommand %q", fs.Arg(0))
	}
	reg, _, err := loadRegistry()
	if err != nil {
		return err
	}
	for _, name := range reg.List() {
		fmt.Println(name)
	}
	return nil
}

func cmdAdapt(args []string) error {
	fs := flag.NewFlagSet("adapt", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), "use: adapt canary|diff|snapshot (see mediactl help)\n") }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("missing adapt subcommand")
	}
	switch fs.Arg(0) {
	case "canary":
		return adaptCanary(fs.Args()[1:])
	case "diff":
		return adaptDiff(fs.Args()[1:])
	case "snapshot":
		return adaptSnapshot(fs.Args()[1:])
	default:
		return fmt.Errorf("unknown adapt subcommand %q", fs.Arg(0))
	}
}

func adaptCanary(args []string) error {
	fs := flag.NewFlagSet("adapt canary", flag.ExitOnError)
	offline := fs.Bool("offline", false, "run fixture-based canaries (no network)")
	live := fs.Bool("live", false, "run live canaries (requires secrets/env; inert until the live driver lands)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *offline == *live {
		return fmt.Errorf("exactly one of --offline / --live is required")
	}
	if *live {
		return fmt.Errorf("live canary driver not implemented yet (see docs/CANARY.md): use --offline")
	}
	_, runner, err := loadRegistry()
	if err != nil {
		return err
	}
	reports, err := runner.RunAllOffline()
	if err != nil {
		return err
	}
	fmt.Print(contracts.Summarize(reports))
	for _, r := range reports {
		if !r.Healthy() {
			return fmt.Errorf("canary reports contain errors (see above)")
		}
	}
	fmt.Printf("offline canary: %d cases healthy\n", len(reports))
	return nil
}

func adaptDiff(args []string) error {
	fs := flag.NewFlagSet("adapt diff", flag.ExitOnError)
	name := fs.String("contract", "", "contract name")
	fixture := fs.String("fixture", "", "JSON payload file to diff against")
	kind := fs.String("kind", "", "items|comments|users|members (empty = all bindings)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *fixture == "" {
		return fmt.Errorf("--contract and --fixture are required")
	}
	reg, _, err := loadRegistry()
	if err != nil {
		return err
	}
	c, ok := reg.Get(*name)
	if !ok {
		return fmt.Errorf("contract %q not registered", *name)
	}
	raw, err := os.ReadFile(*fixture)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	rep := contracts.Diff(c, doc, *kind)
	rep.Observed = *fixture
	fmt.Print(contracts.Summarize([]*contracts.DiffReport{rep}))
	if !rep.Healthy() {
		return fmt.Errorf("diff contains errors")
	}
	return nil
}

func adaptSnapshot(args []string) error {
	fs := flag.NewFlagSet("adapt snapshot", flag.ExitOnError)
	name := fs.String("accept", "", "canary name to regenerate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--accept <name> is required")
	}
	return fmt.Errorf("snapshot --accept %s not implemented yet (see adapt/playbook/AGENTS.md)", *name)
}

func cmdTasks(args []string) error {
	fs := flag.NewFlagSet("tasks", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return fmt.Errorf("tasks subcommand lands with the task-runner PR (use mediad HTTP API for now): %v", fs.Args())
}
