// lab.go — shared plumbing for the offline verification lane
// (`mediactl lab ...`; dispatch lives beside the drill subcommand):
// group-row matrix runs against fixture-driven mock platforms
// (`lab matrix`), comment-author field-completeness audits
// (`lab audit-comments`), and the shared three-valued verdict/report
// plumbing behind them (IR-MM-0001 AC-19; docs/TESTING.md success
// criterion 5: every extreme row ends in {clean success, documented
// skip, fail-closed documented code} — never silent wrong data, never a
// hang).
//
// Environment boundary: A/B/E live-device/live-volume rows require the
// owner environment (ENV-REQ-1..3). This tool executes the offline-
// assertable portion of the matrix and records precise skip reasons for
// the rest; live evidence stays owner-side (INV-4).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Three-valued verdict statuses (TESTING.md criterion 5).
const (
	vClean      = "clean_success"
	vSkip       = "documented_skip"
	vFailClosed = "fail_closed"
)

// Documented skip/error codes carried by rows whose ending is neither a
// plain clean success nor an unexpected failure.
const (
	codeEnvAccounts   = "ENV_REQ_1_ACCOUNTS"
	codeEnvDevices    = "ENV_REQ_2_DEVICES"
	codeEnvVision     = "ENV_REQ_3_VISION_ENDPOINT"
	codeLiveVolume    = "live_volume_only"
	codeLiveChurn     = "live_churn_only"
	codeKeywordSwitch = "live_keyword_switch_only"
	codeNotDeclared   = "contract_not_declared"
	codeRowTimeout    = "row_timeout"
	codeRowPanic      = "row_panic_undocumented"
)

// verdict is one row's ending. For extreme rows exactly the three statuses
// above are legal (TESTING.md); a fail_closed without a documented code is
// an undocumented failure and blocks the report (summary.illegal > 0).
type verdict struct {
	Status  string         `json:"status"`
	Code    string         `json:"code,omitempty"`
	Detail  string         `json:"detail"`
	Metrics map[string]any `json:"metrics,omitempty"`
}

func cleanV(detail string, metrics map[string]any) verdict {
	return verdict{Status: vClean, Detail: detail, Metrics: metrics}
}
func skipV(code, detail string) verdict {
	return verdict{Status: vSkip, Code: code, Detail: detail}
}
func failClosedV(code, detail string) verdict {
	return verdict{Status: vFailClosed, Code: code, Detail: detail}
}

// legalExtreme reports whether v is an acceptable ending for an extreme
// matrix row: clean success, documented skip, or a fail-closed WITH its
// documented code. Silent failure modes never pass.
func legalExtreme(v verdict) bool {
	switch v.Status {
	case vClean, vSkip:
		return true
	case vFailClosed:
		// Safety-net outcomes mean the row never produced a real judgment:
		// they surface loudly instead of passing.
		if isSafetyNetCode(v.Code) {
			return false
		}
		return v.Code != ""
	default:
		return false
	}
}

// safetyNetCodes mark runner-enforced failure modes (panic recovery,
// deadline kill), not row-reachable documented outcomes.
var safetyNetCodes = map[string]bool{
	codeRowPanic:   true,
	codeRowTimeout: true,
}

func isSafetyNetCode(code string) bool { return safetyNetCodes[code] }

// rowResult is one matrix line's judgment record.
type rowResult struct {
	Name       string         `json:"name"`
	Source     string         `json:"source"` // docs/TESTING.md anchor
	Status     string         `json:"status"`
	Code       string         `json:"code,omitempty"`
	Detail     string         `json:"detail"`
	DurationMS int64          `json:"duration_ms"`
	Metrics    map[string]any `json:"metrics,omitempty"`
}

// matrixReport is the machine-readable judgment file written to
// adapt/reports/matrix-<group>-<ts>.json.
type matrixReport struct {
	Card       string         `json:"card"`
	IR         string         `json:"ir"`
	Tool       string         `json:"tool"`
	Group      string         `json:"group"`
	Mode       string         `json:"mode"` // offline_mock | mixed_live_gated
	StartedAt  string         `json:"started_at"`
	FinishedAt string         `json:"finished_at"`
	EnvNote    string         `json:"env_note"`
	Rows       []rowResult    `json:"rows"`
	Summary    map[string]int `json:"summary"`
}

// summarizeRows folds row results into the report summary block: per-status
// counts plus an `illegal` bucket holding anything outside the legal
// endings (bad status, or fail_closed without a documented code). Golden
// rows unexpectedly ending in skip/fail_closed do NOT count as illegal by
// itself — they surface through their own status — but extreme rows must
// never leave the legal triple.
func summarizeRows(rows []rowResult) map[string]int {
	sum := map[string]int{"total": len(rows), vClean: 0, vSkip: 0, vFailClosed: 0, "illegal": 0}
	for _, r := range rows {
		v := verdict{Status: r.Status, Code: r.Code}
		switch {
		case !legalExtreme(v):
			sum["illegal"]++
		case r.Status == vClean:
			sum[vClean]++
		case r.Status == vSkip:
			sum[vSkip]++
		default:
			sum[vFailClosed]++
		}
	}
	return sum
}

// writeReportTo marshals rep (indented) into <dir>/<kind>-<group>-<ts>.json
// and returns the absolute path. Empty dir defaults to adapt/reports — the
// lab evidence home (INV-4: artifacts per run land in adapt/reports).
func writeReportTo(dir, kind, group string, rep any) (string, error) {
	if dir == "" {
		dir = filepath.Join(adaptDir(), "reports")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("reports: %w", err)
	}
	name := fmt.Sprintf("%s-%s-%s.json", kind, group, time.Now().Format("20060102-150405"))
	p := filepath.Join(dir, name)
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, append(raw, '\n'), 0o644); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p, nil
	}
	return abs, nil
}
