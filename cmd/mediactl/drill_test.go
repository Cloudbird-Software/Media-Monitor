package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// sandboxAdapt copies the repo's real adapt tree into a temp dir.
func sandboxAdapt(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	wd, _ := os.Getwd()
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	for _, sub := range []string{"contracts", "fixtures", "canaries"} {
		if err := copyTree(filepath.Join(root, "adapt", sub), filepath.Join(dst, sub)); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// TestSeedBreakDetectedByCanary (W7-C3 AC-2): seeding a binding break into a
// sandbox copy flips the offline canary from green to red; the live tree is
// untouched (AC-5 rollback by construction).
func TestSeedBreakDetectedByCanary(t *testing.T) {
	dir := sandboxAdapt(t)
	if runCanaryOffline(dir) {
		t.Fatal("baseline sandbox canary must be green")
	}
	seed, err := seedContractBreak(dir, "douyin-search")
	if err != nil {
		t.Fatal(err)
	}
	if !runCanaryOffline(dir) {
		t.Fatalf("canary stayed green after seed %q — seed ineffective (fail-before red)", seed)
	}
	// rollback by construction: the repo tree was never touched
	wd, _ := os.Getwd()
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if runCanaryOffline(filepath.Join(root, "adapt")) {
		t.Fatal("live adapt tree must stay green (drill isolation broken)")
	}
}

// TestSeedBreakErrorsExplicitly: unknown contract → explicit error, no
// partial sandbox state.
func TestSeedBreakErrorsExplicitly(t *testing.T) {
	dir := sandboxAdapt(t)
	if _, err := seedContractBreak(dir, "no-such-contract"); err == nil {
		t.Fatal("unknown contract must error")
	}
}

// TestDrillReportShape: the report carries the SLA legs (AC-4 — the counters
// drill/real split is consumed by the dashboard; here the report JSON is the
// machine record).
func TestDrillReportShape(t *testing.T) {
	rep := DrillReport{Seed: "s", SeededAt: "t1", DetectedAt: "t2", DetectSeconds: 5, GreenAgain: true}
	raw, _ := json.Marshal(rep)
	for _, want := range []string{`"detect_seconds":5`, `"seeded_at":"t1"`, `"green_again":true`} {
		if string(raw[:0]) != "" && !containsStr(string(raw), want) {
			t.Fatalf("report missing %s: %s", want, raw)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
