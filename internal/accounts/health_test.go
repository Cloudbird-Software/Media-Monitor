package accounts

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

// TestClassifyHealthMatrix: the four canonical walk shapes map onto the
// three health forms (W4-C1 AC-2 / BEH-5..7).
func TestClassifyHealthMatrix(t *testing.T) {
	cases := []struct {
		name string
		walk ProbeWalk
		want Health
	}{
		{"auth wall 401", ProbeWalk{Page1Status: 401, Page1Err: errors.New("status 401")}, HealthExpired},
		{"risk wall 403", ProbeWalk{Page1Status: 403, Page1Err: errors.New("status 403")}, HealthExpired},
		{"200 empty page1", ProbeWalk{Page1Status: 200, Page1Empty: true}, HealthExpired},
		{"200 empty at depth", ProbeWalk{Page1Status: 200, Page1HasMore: true, DepthChecked: true, Page2Status: 200, Page2Empty: true}, HealthExpired},
		{"depth auth wall", ProbeWalk{Page1Status: 200, DepthChecked: true, Page2Status: 401, Page2Err: errors.New("status 401")}, HealthExpired},
		{"partial: page2 transport failure", ProbeWalk{Page1Status: 200, DepthChecked: true, Page2Err: errors.New("dial timeout")}, HealthDegraded},
		{"partial: page1 5xx", ProbeWalk{Page1Status: 502, Page1Err: errors.New("status 502")}, HealthDegraded},
		{"single-shot ok", ProbeWalk{Page1Status: 200}, HealthHealthy},
		{"pagination depth ok", ProbeWalk{Page1Status: 200, DepthChecked: true, Page2Status: 200}, HealthHealthy},
	}
	for _, c := range cases {
		if got, _ := ClassifyHealth(c.walk); got != c.want {
			t.Errorf("%s: ClassifyHealth = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSetHealthPersists: health + probe timestamp survive a pool reopen
// (W4-C1 AC-1) and ride the snapshot JSON row.
func TestSetHealthPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "accounts")
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Save(Account{ID: "a1", Platform: "douyin", Cookies: map[string]string{"ttwid": "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetHealth("a1", HealthExpired, "page 2 empty (200 + empty body at depth)"); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	a, ok := p2.Get("a1")
	if !ok {
		t.Fatal("account lost after reopen")
	}
	if a.Health != HealthExpired || a.HealthCheckedAt == 0 || a.HealthDetail == "" {
		t.Fatalf("health not persisted: %+v", a)
	}
	// The snapshot row is JSON and carries the health fields (AC-1).
	raw, _ := json.Marshal(a)
	for _, key := range []string{`"health":"expired"`, `"health_checked_at":`, `"health_detail":`} {
		if string(raw[:]) == "" || !contains(string(raw), key) {
			t.Fatalf("snapshot JSON missing %s: %s", key, raw)
		}
	}
}

// TestSetHealthUnknownAccount: fail-closed on an unknown id.
func TestSetHealthUnknownAccount(t *testing.T) {
	p, err := Open(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.SetHealth("nope", HealthHealthy, ""); err == nil {
		t.Fatal("unknown account must error")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestSaveFreshProcessKeepsExistingAccounts: a fresh Pool instance whose
// first operation is Save must not truncate the snapshot to just the new
// account (holdout-found defect: persist rewrote before hydrating).
func TestSaveFreshProcessKeepsExistingAccounts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "accounts")
	p1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p1.Save(Account{ID: "old-1", Platform: "douyin"}); err != nil {
		t.Fatal(err)
	}
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}
	// Fresh process: first op is Save, no Get/Load first.
	p2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if err := p2.Save(Account{ID: "new-1", Platform: "xhs"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"old-1", "new-1"} {
		if _, ok := p2.Get(want); !ok {
			t.Fatalf("account %q lost after fresh-process save (snapshot truncation)", want)
		}
	}
}
