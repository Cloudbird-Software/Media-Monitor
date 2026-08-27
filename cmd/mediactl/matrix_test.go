package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// adaptRootForTest resolves the repo adapt tree from this package.
func adaptRootForTest() string {
	return filepath.Join("..", "..", "adapt")
}

func rowByName(rows []matrixRow, name string) *matrixRow {
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

// TestDecideDeviceSmokeTable: pure E-group decision logic — without an adb
// binary or attached devices it must document-skip with ENV-REQ-2 codes;
// the live-open gate only clears when both are present and explicitly
// enabled (sandbox default: never).
func TestDecideDeviceSmokeTable(t *testing.T) {
	cases := []struct {
		found    bool
		devices  int
		live     bool
		wantCode string
		cleanOK  bool
	}{
		{false, 0, false, codeEnvDevices, false},
		{true, 0, false, codeEnvDevices, false},
		{true, 2, false, codeEnvDevices, false}, // gate closed even w/ devices
		{true, 2, true, "", true},               // owner lane: live attempt
	}
	for i, tc := range cases {
		v := decideDeviceSmoke(tc.found, tc.devices, tc.live)
		if tc.cleanOK && v.Status != vClean {
			t.Fatalf("case %d: want live attempt (clean), got %+v", i, v)
		}
		if !tc.cleanOK && v.Status != vSkip {
			t.Fatalf("case %d: want documented skip, got %+v", i, v)
		}
		if !tc.cleanOK && v.Code != tc.wantCode {
			t.Fatalf("case %d: code=%q want %q", i, v.Code, tc.wantCode)
		}
		if !legalExtreme(v) {
			t.Fatalf("case %d verdict illegal: %+v", i, v)
		}
	}
}

// runGroupForTest is the shared harness: one mock-platform group run with
// reports to a throwaway dir and a per-test deadline of safety.
func runGroupForTest(t *testing.T, group string) matrixReport {
	t.Helper()
	done := make(chan struct{})
	var rep matrixReport
	var err error
	go func() {
		defer close(done)
		rep, _, err = runMatrixGroup(runMatrixOpts{
			Group: group, AdaptRoot: adaptRootForTest(), WriteDir: t.TempDir(),
		})
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatalf("group %s exceeded e2e deadline — hang risk realized", group)
	}
	if err != nil {
		t.Fatalf("runMatrixGroup(%s): %v", group, err)
	}
	return rep
}

func assertLegalEndings(t *testing.T, rep matrixReport) {
	t.Helper()
	for _, r := range rep.Rows {
		v := verdict{Status: r.Status, Code: r.Code}
		if !legalExtreme(v) {
			t.Errorf("row %s ended outside the legal triple: status=%q code=%q detail=%s",
				r.Name, r.Status, r.Code, firstLine(r.Detail))
		}
		if t.Failed() {
			t.FailNow()
		}
	}
}

// TestUserPostsMatrixRowsOffline: the IR-new matrix line's three assertions
// (backtrack depth, threshold early stop, window cutoff), plus resumable
// cursors and the undeclared-platform fail-closed demo, all offline.
func TestUserPostsMatrixRowsOffline(t *testing.T) {
	rep := runGroupForTest(t, "user_posts")
	assertLegalEndings(t, rep)

	names := map[string]string{}
	for _, r := range rep.Rows {
		names[r.Name] = r.Status
	}
	upDepth, ok := names["up-douyin-backtrack-depth3"]
	_ = upDepth
	if !ok || names["up-douyin-backtrack-depth3"] != vClean {
		t.Fatalf("depth row not clean: %+v", names)
	}
	if names["up-xhs-backtrack-depth3"] != vClean {
		t.Fatalf("xhs parity row not clean: %v", names["up-xhs-backtrack-depth3"])
	}
	if names["up-douyin-threshold-early-stop"] != vClean {
		t.Fatalf("early-stop row not clean")
	}
	if names["up-douyin-window-cutoff"] != vClean {
		t.Fatalf("window row not clean")
	}
	if names["up-douyin-cursor-resume-once"] != vClean {
		t.Fatalf("resume row not clean")
	}
	if names["fc-up-kuaishou-not-declared"] != vFailClosed {
		t.Fatalf("not-declared row must be fail_closed")
	}
	// Specific behavioral metrics prove the backtrack params reached the
	// engine rather than being swallowed.
	for _, r := range rep.Rows {
		switch r.Name {
		case "up-douyin-backtrack-depth3":
			if m(r, "pages") != float64(3) || m(r, "items") != float64(6) {
				t.Fatalf("depth metrics wrong: %v", r.Metrics)
			}
		case "up-douyin-threshold-early-stop":
			if m(r, "items_emitted") != float64(2) || m(r, "requests") != float64(1) {
				t.Fatalf("early-stop metrics wrong: %v", r.Metrics)
			}
		case "fc-up-kuaishou-not-declared":
			if r.Code != codeNotDeclared {
				t.Fatalf("not-declared code = %q", r.Code)
			}
		}
	}
	if rep.Summary[vClean] != 5 || rep.Summary[vFailClosed] != 1 || rep.Summary["illegal"] != 0 {
		t.Fatalf("user_posts summary wrong: %v (names=%v)", rep.Summary, names)
	}
}

func m(r rowResult, key string) float64 {
	switch v := r.Metrics[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return -1
	}
}

// TestGoldenMatrixRowVerdicts: search + comments golden lines pass offline
// with their evidence metrics.
func TestGoldenMatrixRowVerdicts(t *testing.T) {
	for _, group := range []string{"a", "b"} {
		rep := runGroupForTest(t, group)
		assertLegalEndings(t, rep)
		if got := len(rep.Rows); got == 0 {
			t.Fatalf("%s produced no rows", group)
		}
		golden := 0
		skips := 0
		for _, r := range rep.Rows {
			switch {
			case strings.HasPrefix(r.Name, "ext-"):
				if r.Status != vClean {
					t.Fatalf("%s: extreme row not clean_success offline: %s(%s) %s",
						r.Name, r.Status, r.Code, firstLine(r.Detail))
				}
			case strings.HasPrefix(r.Name, "skip-"):
				skips++
				if r.Status != vSkip || r.Code == "" {
					t.Fatalf("%s: volume-bound row must carry its skip code", r.Name)
				}
			default:
				golden++
				if r.Status != vClean {
					t.Fatalf("%s: golden row red: %s %s", r.Name, r.Status, firstLine(r.Detail))
				}
			}
		}
		minGolden := 3 // a-group carries exactly the three platform search lines
		if group == "b" {
			minGolden = 4 // three comment walks + reply fan-out
		}
		if golden < minGolden {
			t.Fatalf("%s: expected >=%d golden rows, saw %d (%+v)", group, minGolden, golden, rep.Rows)
		}
		if skips == 0 {
			t.Fatalf("%s: expected at least one owner-env documented skip", group)
		}
	}
}

// TestCommentsGoldenCarriesFieldAudit: the B-group hot-item walk embeds the
// AC-19 author completeness metric >= 90%.
func TestCommentsGoldenCarriesFieldAudit(t *testing.T) {
	rc := newRowCtx(func() *mockPlatform { mp := newMockPlatform(); mp.Start(); return mp }(), adaptRootForTest())
	defer rc.mp.Close()
	v := rowCommentsGolden(rc, douyinP, 3)
	if v.Status != vClean {
		t.Fatalf("douyin comments walk red: %+v (%s)", v, v.Detail)
	}
	pct, _ := v.Metrics["author_field_pct"].(float64)
	if pct < defaultMinCompleteness {
		t.Fatalf("author_field_pct=%.1f below AC-19 floor", pct)
	}
}

// TestDeviceAndEnvRowsDocumentSkipsSandbox: in a sandbox (no adb devices,
// no vision endpoint, gate off) every E row ends as a documented skip.
func TestDeviceAndEnvRowsDocumentSkipsSandbox(t *testing.T) {
	t.Setenv(liveGateEnv, "")
	t.Setenv(visionEnvVar, "")
	mp := newMockPlatform()
	defer mp.Close()
	mp.Start()
	rc := newRowCtx(mp, adaptRootForTest())
	for _, name := range []string{"e-devices-smoke-tap-swipe-text-screencap-uidump",
		"e-vision-multistep-config"} {
		row := rowByName(rowsGroupE(), name)
		if row == nil {
			t.Fatalf("row %s missing from group E", name)
		}
		v := row.Run(rc)
		if v.Status != vSkip || v.Code == "" {
			t.Fatalf("%s must document-skip in sandbox, got %+v", name, v)
		}
	}
}

// TestExtremesThreeValuedClosure: whatever surprises a hostile fixture
// throws, every extreme row still ends inside {clean, skip, fail-closed}.
// The placeholder-audit and emoji roundtrips double-check no silent data
// corruption slips through binding.
func TestExtremesThreeValuedClosure(t *testing.T) {
	mp := newMockPlatform()
	defer mp.Close()
	mp.Start()
	rc := newRowCtx(mp, adaptRootForTest())
	rows := append(rowsGroupA(), rowsGroupB()...)
	for _, row := range rows {
		if !strings.HasPrefix(row.Name, "ext-") {
			continue
		}
		started := time.Now()
		v := func() (caught verdict) {
			defer func() {
				if p := recover(); p != nil {
					caught = verdict{Status: vFailClosed, Code: codeRowPanic,
						Detail: "panic"}
				}
			}()
			return row.Run(rc)
		}()
		if !legalExtreme(v) {
			t.Fatalf("%s illegal ending: %+v", row.Name, v)
		}
		if elapsed := time.Since(started); elapsed > rowTimeout {
			t.Fatalf("%s ran %v beyond deadline", row.Name, elapsed)
		}
	}
}

// TestUnknownGroupFailsClosed: garbage input produces an explicit error,
// not an empty report.
func TestUnknownGroupFailsClosed(t *testing.T) {
	if _, _, err := runMatrixGroup(runMatrixOpts{Group: "nope", AdaptRoot: adaptRootForTest()}); err == nil {
		t.Fatal("unknown group accepted")
	}
	if _, err := rowsForGroup(""); err == nil {
		t.Fatal("empty group accepted")
	}
}
