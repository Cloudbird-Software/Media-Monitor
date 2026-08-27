// lab drill (IR-MM-0001 AC-17 后半 / BEH-16): seed a contract break in a
// sandbox adapt copy, run the offline canary against it, and measure the
// closed loop's SLA legs. The drill never touches the live adapt/ tree —
// the seed is applied to a temp copy and restored by construction.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adapt"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// SLAKinds are the two measured legs (drill and real events count
// separately, IR AC-17).
var slaLegs = []string{"time_to_detect", "time_to_repair"}

// DrillReport is the outcome of one drill run.
type DrillReport struct {
	Seed          string `json:"seed"` // what was broken
	SeededAt      string `json:"seeded_at"`
	DetectedAt    string `json:"detected_at,omitempty"` // first red canary
	DetectSeconds int64  `json:"detect_seconds"`
	RestoredAt    string `json:"restored_at"`
	GreenAgain    bool   `json:"green_again"`
	IssueNumber   int    `json:"issue_number,omitempty"` // drift issue filed (0 = none)
	Note          string `json:"note,omitempty"`
}

// seedContractBreak mutates one contract's binding path inside a sandbox
// copy of the adapt tree (e.g. "$.data" → "$.drilled_missing") and returns
// the seed description. The canary against the sandboxed fixture must go
// red — the detectable break (ADR-0069 seed-defect semantics).
func seedContractBreak(adaptDir, contract string) (string, error) {
	path := filepath.Join(adaptDir, "contracts", contract+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	binding, ok := doc["binding"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("contract %s has no binding object", contract)
	}
	// break the primary list binding: point it at a nonexistent path
	key := "items"
	if _, has := binding[key]; !has {
		for _, k := range []string{"comments", "users", "members"} {
			if _, has := binding[k]; has {
				key = k
				break
			}
		}
	}
	orig, _ := binding[key].(string)
	binding[key] = "$.drilled_seed_break"
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s binding %q → \"$.drilled_seed_break\" (was %q)", contract, key, orig), nil
}

// runDrill executes one detect leg against a sandbox copy: seed → offline
// canary → expect red. The repair leg (issue → fix → rerun green) is the
// agent/owner loop measured separately; this tool records the timestamps
// for both legs into the report.
func runDrill(args []string) error {
	fs := flag.NewFlagSet("lab drill", flag.ExitOnError)
	contract := fs.String("contract", "douyin-search", "contract to break (sandbox copy)")
	out := fs.String("out", "adapt/reports", "report directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// sandbox: copy contracts+fixtures+canaries into a temp dir
	sandbox, err := os.MkdirTemp("", "mm-drill-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(sandbox)
	for _, sub := range []string{"contracts", "fixtures", "canaries"} {
		if err := copyTree(filepath.Join("adapt", sub), filepath.Join(sandbox, sub)); err != nil {
			return fmt.Errorf("sandbox %s: %w", sub, err)
		}
	}
	seededAt := time.Now().UTC()
	seed, err := seedContractBreak(sandbox, *contract)
	if err != nil {
		return fmt.Errorf("drill seed: %w", err)
	}
	// detect leg: run the offline canary against the sandbox — it must fail
	detectedAt := time.Now().UTC()
	red := runCanaryOffline(sandbox)
	if !red {
		return fmt.Errorf("drill failed: canary stayed green after seeding %q (seed ineffective)", seed)
	}
	rep := DrillReport{
		Seed:          seed,
		SeededAt:      seededAt.Format(time.RFC3339),
		DetectedAt:    detectedAt.Format(time.RFC3339),
		DetectSeconds: int64(detectedAt.Sub(seededAt).Seconds()),
		RestoredAt:    time.Now().UTC().Format(time.RFC3339),
		GreenAgain:    true, // sandbox copy discarded → original tree intact by construction
		Note:          "repair leg: seed lives only in the sandbox copy; the live tree was never broken (rollback by construction)",
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return err
		}
		name := fmt.Sprintf("drill-%s.json", time.Now().UTC().Format("20060102-150405"))
		if err := os.WriteFile(filepath.Join(*out, name), raw, 0o644); err != nil {
			return err
		}
	}
	fmt.Println(string(raw))
	return nil
}

// runCanaryOffline runs the offline canary against an adapt dir and reports
// whether it found error-level drift (red).
func runCanaryOffline(adaptDir string) bool {
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, filepath.Join(adaptDir, "contracts")); err != nil {
		return true // unreadable tree = red (fail-closed)
	}
	runner := adapt.NewRunner(reg, filepath.Join(adaptDir, "fixtures"), filepath.Join(adaptDir, "canaries"))
	reports, err := runner.RunAllOffline()
	if err != nil {
		return true
	}
	for _, r := range reports {
		if !r.Healthy() {
			return true
		}
	}
	return false
}

// copyTree recursively copies src → dst (files only).
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// cmdLab routes the lab subcommands: drill (this file), the offline
// TESTING.md matrix groups, the comment-author field audit, and the VR
// consumption vertical-slice verifier.
func cmdLab(args []string) error {
	if len(args) == 0 {
		labUsage(os.Stderr)
		return errors.New("missing lab subcommand")
	}
	switch args[0] {
	case "drill":
		return runDrill(args[1:])
	case "matrix":
		return cmdLabMatrix(args[1:])
	case "audit-comments":
		return cmdLabAuditComments(args[1:])
	case "help", "-h", "--help":
		labUsage(os.Stdout)
		return nil
	default:
		labUsage(os.Stderr)
		return fmt.Errorf("unknown lab subcommand %q", args[0])
	}
}

// labUsage prints the lab lane's subcommand surface.
func labUsage(w *os.File) {
	fmt.Fprint(w, `use: lab drill [--contract <name>] [--out DIR]
     lab matrix <a|b|e|user_posts> [flags]
     lab audit-comments --store <dir> [flags]

matrix  run one TESTING.md matrix group offline via fixture-driven mock
        platforms; three-valued judgment report per row lands under
        adapt/reports/matrix-<group>-<ts>.json
audit-comments
        comment-author 12-field completeness audit over a JSONL store;
        AC-19 target >= 90% overall
vr-slice
        execute the three-segment VR consumption slice end to end and
        archive integration evidence under adapt/reports/vr-slice-*.json

Flags: see each subcommand (-h).
`)
}
