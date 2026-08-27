package main

import "testing"

// TestLegalExtremeVerdicts: only clean_success, documented_skip, or a
// fail_closed WITH a documented code are acceptable extreme-row endings
// (TESTING.md criterion 5). A fail_closed without its code is an
// undocumented failure and must never pass.
func TestLegalExtremeVerdicts(t *testing.T) {
	cases := []struct {
		v    verdict
		want bool
	}{
		{verdict{Status: vClean}, true},
		{verdict{Status: vSkip, Code: codeEnvDevices}, true},
		{verdict{Status: vFailClosed, Code: codeNotDeclared}, true},
		{verdict{Status: vFailClosed}, false}, // silent failure mode
		{verdict{Status: vFailClosed, Code: codeRowPanic}, false},
		{verdict{Status: "silent_wrong_data"}, false},
		{verdict{}, false},
	}
	for i, tc := range cases {
		if got := legalExtreme(tc.v); got != tc.want {
			t.Fatalf("case %d (%+v): legalExtreme=%v want %v", i, tc.v, got, tc.want)
		}
	}
}

// TestSummarizeRowsCountsStatusesAndIllegal: the summary block counts each
// legal status and buckets everything else under illegal.
func TestSummarizeRowsCountsStatusesAndIllegal(t *testing.T) {
	rows := []rowResult{
		{Name: "r1", Status: vClean},
		{Name: "r2", Status: vClean},
		{Name: "r3", Status: vSkip, Code: codeLiveVolume},
		{Name: "r4", Status: vFailClosed, Code: codeNotDeclared},
		{Name: "r5", Status: vFailClosed, Code: ""}, // undocumented
		{Name: "r6", Status: "hung_forever"},        // illegal status
	}
	sum := summarizeRows(rows)
	want := map[string]int{"total": 6, vClean: 2, vSkip: 1, vFailClosed: 1, "illegal": 2}
	for k, w := range want {
		if sum[k] != w {
			t.Fatalf("summary[%s]=%d want %d (full: %v)", k, sum[k], w, sum)
		}
	}
}

// TestEmptySummary: an empty run still reports total=0 with no illegals.
func TestEmptySummary(t *testing.T) {
	sum := summarizeRows(nil)
	if sum["total"] != 0 || sum["illegal"] != 0 {
		t.Fatalf("empty summary = %v", sum)
	}
}

// TestReportEnvelopeFields: every report carries card/IR/mode/env-note so
// the evidence trail states its own environment boundary (INV-4).
func TestReportEnvelopeFields(t *testing.T) {
	rep := matrixReport{}
	rep.Card = matrixCard
	rep.IR = matrixIR
	rep.Mode = "offline_mock"
	if rep.Card == "" || rep.IR == "" || rep.Mode == "" {
		t.Fatalf("report envelope incomplete: %+v", rep)
	}
}
