package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Cloudbird-Software/Media-Monitor/internal/netcapture"
)

// netcapture.go — query/export of persisted capture sessions. Sessions are
// recorded by mediad / programmatic writers into the store dir; this command
// only reads them (a headless CLI has no CDP capture capability, so there is
// deliberately no record subcommand).

// netcaptureDir resolves the persistent netcapture store dir:
// $MEDIAMON_NETCAPTURE_DIR override, default data/netcapture.
func netcaptureDir() string {
	if d := os.Getenv("MEDIAMON_NETCAPTURE_DIR"); d != "" {
		return d
	}
	return filepath.Join("data", "netcapture")
}

// cmdNetcapture dispatches netcapture subcommands.
func cmdNetcapture(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use: netcapture list | netcapture convert <har> | netcapture export --project <name> --out <file.har>")
	}
	switch args[0] {
	case "list":
		return netcaptureList(args[1:])
	case "convert":
		return netcaptureConvertCmd(args[1:])
	case "export":
		return netcaptureExport(args[1:])
	default:
		return fmt.Errorf("unknown netcapture subcommand %q", args[0])
	}
}

// netcaptureList prints every persisted session with its entry count.
func netcaptureList(args []string) error {
	fs := flag.NewFlagSet("netcapture list", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	mgr, err := netcapture.NewManagerDir(netcaptureDir())
	if err != nil {
		return err
	}
	defer mgr.Close()
	names, err := mgr.List()
	if err != nil {
		return err
	}
	for _, n := range names {
		entries := 0
		s, ok, err := mgr.Load(n)
		if err != nil {
			return err
		}
		if ok {
			entries = len(s.Entries)
		}
		fmt.Printf("%s\tentries=%d\n", n, entries)
	}
	return nil
}

// netcaptureExport writes one persisted session as a HAR file.
func netcaptureExport(args []string) error {
	fs := flag.NewFlagSet("netcapture export", flag.ExitOnError)
	project := fs.String("project", "", "session project name (required)")
	out := fs.String("out", "", "output HAR file (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *out == "" {
		return fmt.Errorf("--project and --out are required")
	}
	mgr, err := netcapture.NewManagerDir(netcaptureDir())
	if err != nil {
		return err
	}
	defer mgr.Close()
	s, ok, err := mgr.Load(*project)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no session %q under %s (sessions are recorded by mediad / programmatic writers)", *project, netcaptureDir())
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := s.WriteHAR(f); err != nil {
		return err
	}
	fmt.Printf("exported session %q (%d entries) -> %s\n", *project, len(s.Entries), *out)
	return nil
}

// netcaptureConvertCmd implements `mediactl netcapture convert <har-file>`:
// HAR → candidate fixtures (redacted) → contract patch proposals. Proposals
// print to stdout as JSON; nothing is written into adapt/ (proposal ≠
// change).
func netcaptureConvertCmd(args []string) error {
	fs := flag.NewFlagSet("netcapture convert", flag.ExitOnError)
	outDir := fs.String("out", "", "directory to write candidate fixture files (default: stdout only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: netcapture convert <har-file> [--out <dir>]")
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var har netcapture.HAR
	if err := json.Unmarshal(raw, &har); err != nil {
		return fmt.Errorf("har parse: %w", err)
	}
	reg, _, err := loadRegistry()
	if err != nil {
		return err
	}
	cands, errs := netcapture.ConvertHAR(&har, reg)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "skip: %v\n", e)
	}
	if *outDir != "" && len(cands) > 0 {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			return err
		}
	}
	for i, c := range cands {
		enc, _ := json.Marshal(c)
		fmt.Println(string(enc))
		if *outDir != "" {
			name := fmt.Sprintf("candidate-%d.json", i+1)
			if c.Contract != "" {
				name = c.Contract + ".candidate.json"
			}
			if err := os.WriteFile(filepath.Join(*outDir, name), enc, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}
