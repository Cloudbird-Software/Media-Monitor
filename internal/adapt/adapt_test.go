package adapt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// testDirs resolves the repository adapt/ tree relative to this package.
func testDirs(t *testing.T) (contractsDir, fixtures, canaries string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	return filepath.Join(root, "adapt", "contracts"),
		filepath.Join(root, "adapt", "fixtures"),
		filepath.Join(root, "adapt", "canaries")
}

func TestOfflineCanariesHealthy(t *testing.T) {
	cdir, fdir, cdir2 := testDirs(t)
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, cdir); err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) == 0 {
		t.Fatal("contract registry empty")
	}
	r := NewRunner(reg, fdir, cdir2)
	reports, err := r.RunAllOffline()
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) == 0 {
		t.Fatal("no canary cases executed")
	}
	if summary := contracts.Summarize(reports); strings.Contains(summary, "UNHEALTHY") {
		t.Fatalf("canaries unhealthy:\n%s", summary)
	}
}

func TestBrokenFixtureIsUnhealthy(t *testing.T) {
	cdir, fdir, cdir2 := testDirs(t)
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, cdir); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(reg, fdir, cdir2)
	rep, err := r.RunOffline("douyin-comments-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Healthy() {
		t.Fatalf("baseline fixture should be healthy: %s", contracts.Summarize([]*contracts.DiffReport{rep}))
	}

	// Copy the real fixtures into a scratch dir, drift one payload, and
	// verify the canary turns unhealthy (binder must fail closed).
	tmp := t.TempDir()
	copyFile := func(from, to string) {
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(filepath.Join(fdir, "douyin-comments.1.json"), filepath.Join(tmp, "douyin-comments.1.json"))
	copyFile(filepath.Join(fdir, "douyin-user.1.json"), filepath.Join(tmp, "douyin-user.1.json"))

	r2 := NewRunner(reg, tmp, cdir2)
	okRep, err := r2.RunOffline("douyin-comments-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !okRep.Healthy() {
		t.Fatalf("copied fixture should stay healthy: %s", contracts.Summarize([]*contracts.DiffReport{okRep}))
	}
	if err := os.WriteFile(filepath.Join(tmp, "douyin-comments.1.json"), []byte(`{"something_else": [1]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bad, err := r2.RunOffline("douyin-comments-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if bad.Healthy() {
		t.Fatalf("drifted fixture must be UNHEALTHY, got: %s", contracts.Summarize([]*contracts.DiffReport{bad}))
	}
}

func TestValidateUnknownContract(t *testing.T) {
	cdir, fdir, cdir2 := testDirs(t)
	reg := contracts.NewRegistry()
	_ = contracts.LoadDir(reg, cdir)
	r := NewRunner(reg, fdir, cdir2)
	if _, err := r.RunOffline("no-such-canary"); err == nil {
		t.Fatal("expected error for unknown canary")
	}
}
