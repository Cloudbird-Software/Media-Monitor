// Package adapt implements the offline adaptation harness: loading canary
// cases, validating contract/fixture coherence, and producing machine
// -actionable drift reports via contracts.Diff.
package adapt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// Runner executes canary cases against contracts + fixtures.
type Runner struct {
	Registry    *contracts.Registry
	FixturesDir string
	CanariesDir string
}

// NewRunner builds the harness runner.
func NewRunner(registry *contracts.Registry, fixturesDir, canariesDir string) *Runner {
	return &Runner{Registry: registry, FixturesDir: fixturesDir, CanariesDir: canariesDir}
}

// CanaryCase is one golden verification unit.
type CanaryCase struct {
	Name     string   `json:"name"`
	Contract string   `json:"contract"`
	Kind     string   `json:"kind"` // items|comments|users|members
	Fixture  string   `json:"fixture"`
	Expect   []string `json:"expect"`
}

// LoadCanaries reads a canary file holding {"canaries":[...]}.
func LoadCanaries(file string) ([]CanaryCase, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Canaries []CanaryCase `json:"canaries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("canaries %s: %w", file, err)
	}
	return doc.Canaries, nil
}

// Canaries loads every file in the canary dir.
func (r *Runner) Canaries() ([]CanaryCase, error) {
	entries, err := os.ReadDir(r.CanariesDir)
	if err != nil {
		return nil, err
	}
	var out []CanaryCase
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		cs, err := LoadCanaries(filepath.Join(r.CanariesDir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, cs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Validate checks registry/canary/fixture coherence before running.
func (r *Runner) Validate() error {
	if len(r.Registry.List()) == 0 {
		return fmt.Errorf("contract registry is empty")
	}
	cs, err := r.Canaries()
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		return fmt.Errorf("no canary cases found in %s", r.CanariesDir)
	}
	for _, c := range cs {
		if _, ok := r.Registry.Get(c.Contract); !ok {
			return fmt.Errorf("canary %s: contract %q not registered", c.Name, c.Contract)
		}
		if _, err := os.Stat(filepath.Join(r.FixturesDir, c.Fixture)); err != nil {
			return fmt.Errorf("canary %s: fixture %q missing", c.Name, c.Fixture)
		}
		switch c.Kind {
		case "", "items", "comments", "users", "members", "meta":
		default:
			return fmt.Errorf("canary %s: unknown kind %q", c.Name, c.Kind)
		}
	}
	return nil
}

// RunOffline executes a single canary case and returns the drift report.
func (r *Runner) RunOffline(name string) (*contracts.DiffReport, error) {
	cs, err := r.Canaries()
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if c.Name == name {
			return r.runCase(c)
		}
	}
	return nil, fmt.Errorf("canary %q not found", name)
}

// RunAllOffline executes every canary case.
func (r *Runner) RunAllOffline() ([]*contracts.DiffReport, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	cs, err := r.Canaries()
	if err != nil {
		return nil, err
	}
	var out []*contracts.DiffReport
	for _, c := range cs {
		rep, err := r.runCase(c)
		if err != nil {
			return nil, err
		}
		rep.Observed = c.Fixture
		out = append(out, rep)
	}
	return out, nil
}

func (r *Runner) runCase(c CanaryCase) (*contracts.DiffReport, error) {
	contract, ok := r.Registry.Get(c.Contract)
	if !ok {
		return nil, fmt.Errorf("contract %q not registered", c.Contract)
	}
	raw, err := os.ReadFile(filepath.Join(r.FixturesDir, c.Fixture))
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fixture %s: %w", c.Fixture, err)
	}
	rep := contracts.Diff(contract, doc, c.Kind)
	rep.Observed = c.Fixture
	for _, key := range c.Expect {
		if _, ok := doc[key]; !ok {
			rep.Issues = append(rep.Issues, contracts.DriftIssue{
				Severity: "error", Code: "required_field_missing",
				Detail: fmt.Sprintf("canary expect key %q absent from fixture root", key),
			})
		}
	}
	return rep, nil
}
