package collect_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchCheckBlocksUpstreamImports proves the INV-3 guard (internal/ may
// never import upstream/ — submodules are observation copies, ADR-0099)
// actually rejects a violating sample (fail-closed, W5-C1 AC-3).
func TestArchCheckBlocksUpstreamImports(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "quality", "arch-check.sh")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// baseline: the guard passes on the clean tree
	if out, err := runScript(script); err != nil {
		t.Fatalf("baseline arch-check failed: %v\n%s", err, out)
	}
	// violation sample: a temp file inside internal/ importing upstream/
	violation := filepath.Join(root, "internal", "collect", "zz_violation_sample_test.go")
	if err := writeViolations(violation); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removeViolations(violation) }()
	out, err := runScript(script)
	if err == nil {
		t.Fatalf("arch-check accepted an upstream import (guard inert)\n%s", out)
	}
	if !strings.Contains(out, "upstream") {
		t.Fatalf("arch-check failure did not name upstream:\n%s", out)
	}
}

func writeViolations(path string) error {
	// The offending import is assembled at runtime: the literal must not
	// appear in this file's own source, or the guard (rightly) flags the
	// test itself during the baseline run.
	up := "github.com/Cloudbird-Software/Media-Monitor/" + "upstream/" + "vendor/f2"
	src := "package collect\n\nimport _ \"" + up + "\"\n\nvar _ = 1\n"
	return os.WriteFile(path, []byte(src), 0o644)
}

func removeViolations(path string) error { return os.Remove(path) }

func runScript(script string) (string, error) {
	cmd := exec.Command("bash", script)
	cmd.Dir = filepath.Dir(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	// internal/collect → repo root is two levels up
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// TestUpstreamRegistryPinsResolved: every registry entry carries a resolved
// pin and a license verdict — zero TBD (W5-C1 AC-2).
func TestUpstreamRegistryPinsResolved(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "upstream", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Entries []struct {
			Slug string `json:"slug"`
			Pin  struct {
				Type string `json:"type"`
				Ref  string `json:"ref"`
			} `json:"pin"`
			License struct {
				Spdx    string `json:"spdx"`
				Verdict string `json:"verdict"`
			} `json:"license"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Entries) < 6 {
		t.Fatalf("entries = %d, want the six-plus registry", len(reg.Entries))
	}
	for _, e := range reg.Entries {
		if e.Pin.Ref == "" || strings.Contains(e.Pin.Ref, "TBD") {
			t.Errorf("%s: pin unresolved: %+v", e.Slug, e.Pin)
		}
		if e.License.Spdx == "TBD" || e.License.Verdict == "pending" || e.License.Verdict == "" {
			t.Errorf("%s: license verdict unresolved: %+v", e.Slug, e.License)
		}
	}
}
