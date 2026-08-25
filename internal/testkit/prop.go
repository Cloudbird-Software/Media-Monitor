// Package testkit provides a tiny deterministic property-testing harness for
// invariants: every property run is seeded so failures are reproducible, and
// the random generators (R) produce shapes that survive JSON round-trips.
package testkit

import (
	"fmt"
	"math/rand"
	"testing"
)

// R is a seeded random source handed to property invariants. All randomness
// derives from NewR's seed, so a failing iteration reproduces exactly.
type R struct{ R *rand.Rand }

// NewR returns an R backed by a new rand.Rand seeded with seed.
func NewR(seed int64) *R { return &R{R: rand.New(rand.NewSource(seed))} }

// Int63n returns a non-negative pseudo-random int64 in [0, n). It panics if
// n <= 0 (same contract as math/rand).
func (r *R) Int63n(n int64) int64 { return r.R.Int63n(n) }

// Bytes returns a random byte slice of length in [0, maxLen] (any bytes,
// including zeroes; not JSON-safe by itself).
func (r *R) Bytes(maxLen int) []byte {
	if maxLen < 0 {
		maxLen = 0
	}
	n := int(r.Int63n(int64(maxLen + 1)))
	b := make([]byte, n)
	_, _ = r.R.Read(b)
	return b
}

// Prop is one named invariant. Inv returns "" on success or a failure message.
type Prop struct {
	Name string
	Inv  func(*R) string
}

// Run runs every prop for iters iterations, per prop in its own subtest, all
// from the same seed. For each failing iteration the iteration number is
// reported via t.Errorf (name + iteration index). Run itself never panics on
// a failing invariant.
func Run(t *testing.T, seed int64, iters int, props []Prop) {
	t.Helper()
	for _, p := range props {
		p := p
		t.Run(p.Name, func(t *testing.T) {
			t.Helper()
			fails := check(p, seed, iters)
			if len(fails) > 0 {
				t.Errorf("prop %s: %d/%d iterations failed, failing iteration index(es): %v (first: %d)",
					p.Name, len(fails), iters, fails, fails[0])
			}
		})
	}
}

// check evaluates one invariant without touching the testing.T, returning the
// iteration indices that failed. Shared by Run (which reports via t.Errorf)
// and RunExpectFail (used by negative tests of the harness itself).
func check(p Prop, seed int64, iters int) []int {
	var fails []int
	r := NewR(seed)
	for i := 0; i < iters; i++ {
		if msg := p.Inv(r); msg != "" {
			fails = append(fails, i)
		}
	}
	return fails
}

// RunExpectFail asserts that each prop fails at least once across iters
// iterations. It is the negative mirror of Run for harness testing: pass
// intentionally-bad invariants here; a prop that never fails is a bug in the
// failure-detection machinery, which is reported with t.Errorf.
func RunExpectFail(t *testing.T, seed int64, iters int, props []Prop) {
	t.Helper()
	for _, p := range props {
		p := p
		t.Run("expect_fail_"+p.Name, func(t *testing.T) {
			t.Helper()
			if fails := check(p, seed, iters); len(fails) == 0 {
				t.Errorf("prop %s: expected at least one failing iteration but the invariant never failed", p.Name)
			}
		})
	}
}

// jsonChars are printable ASCII characters used by Map so that generated
// values marshal/unmarshal byte-identically (no invalid UTF-8, no escapes).
const jsonChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 _-.,;:!?()"

// Map returns a random nested JSON object up to depth levels of nesting with
// arrays, and keys/values within [1, keys] and [1, vals] bounds (min 1 each).
// Value types: string, int (|v| < 2^40 so it round-trips through JSON
// float64 exactly), float, bool, nil, nested object, array. The result is
// guaranteed to survive encoding/json round-trips value-for-value.
func (r *R) Map(depth, keys, vals int) map[string]any {
	return r.obj(depth, keys, vals).(map[string]any)
}

func (r *R) obj(depth, keys, vals int) any {
	if keys < 1 {
		keys = 1
	}
	if vals < 1 {
		vals = 1
	}
	n := 1 + int(r.Int63n(int64(keys)))
	m := make(map[string]any, n)
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("k%d", i)] = r.val(depth-1, keys, vals)
	}
	return m
}

func (r *R) val(depth, keys, vals int) any {
	// Depth 0 (or 1 below root) may not nest further.
	nested := depth > 0
	switch int(r.Int63n(7)) {
	case 0: // string
		return r.str()
	case 1: // int, small enough for exact float64 round-trip
		v := r.R.Int63n(1 << 40)
		if r.R.Int63n(2) == 0 {
			v = -v
		}
		return v
	case 2: // float
		return float64(r.R.Int63n(1_000_000)) / float64(1+int(r.R.Int63n(1000)))
	case 3: // bool
		return r.R.Int63n(2) == 0
	case 4: // nil
		return nil
	case 5: // nested object
		if nested {
			return r.obj(depth-1, keys, vals)
		}
		return r.str()
	default: // array
		if nested {
			n := 1 + int(r.Int63n(int64(vals)))
			arr := make([]any, n)
			for i := range arr {
				arr[i] = r.val(depth-1, keys, vals)
			}
			return arr
		}
		return r.str()
	}
}

func (r *R) str() string {
	n := 1 + int(r.Int63n(32))
	b := make([]byte, n)
	for i := range b {
		b[i] = jsonChars[int(r.Int63n(int64(len(jsonChars))))]
	}
	return string(b)
}
