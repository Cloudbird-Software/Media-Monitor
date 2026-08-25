package testkit

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestRunPass: a never-failing invariant produces no failures.
func TestRunPass(t *testing.T) {
	Run(t, 42, 100, []Prop{{
		Name: "always_ok",
		Inv:  func(r *R) string { r.Int63n(5); return "" },
	}})
}

// TestRunExpectFail: a deliberately-bad invariant is detected by RunExpectFail
// (which itself fails the test only when detection is broken, i.e. when the
// bad invariant never fails).
func TestRunExpectFail(t *testing.T) {
	RunExpectFail(t, 7, 200, []Prop{{
		Name: "bad_invariant",
		Inv: func(r *R) string {
			if r.Int63n(3) == 0 {
				return "hit the failure branch"
			}
			return ""
		},
	}, {
		Name: "nil_on_first",
		Inv:  func(r *R) string { return "always fails" },
	}})
}

// TestBytesBounds checks the length contract of Bytes.
func TestBytesBounds(t *testing.T) {
	r := NewR(11)
	for i := 0; i < 500; i++ {
		max := int(r.Int63n(1000))
		b := r.Bytes(max)
		if len(b) > max {
			t.Fatalf("len(%d) exceeds max %d", len(b), max)
		}
	}
	if b := r.Bytes(0); len(b) != 0 {
		t.Fatalf("Bytes(0) = len %d, want 0", len(b))
	}
}

// TestMapJSONRoundTrip: every generated map survives a JSON round-trip
// value-for-value (exact re-marshal byte equality).
func TestMapJSONRoundTrip(t *testing.T) {
	r := NewR(20260825)
	for i := 0; i < 200; i++ {
		m := r.Map(4, 6, 5)
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("iter %d: marshal: %v", i, err)
		}
		var back map[string]any
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("iter %d: unmarshal: %v", i, err)
		}
		raw2, err := json.Marshal(back)
		if err != nil {
			t.Fatalf("iter %d: re-marshal: %v", i, err)
		}
		if !reflect.DeepEqual(raw, raw2) {
			t.Fatalf("iter %d: JSON round-trip changed value:\n%v\nvs\n%v", i, string(raw), string(raw2))
		}
	}
}

// TestMapShape: depth and keys bounds hold at the root level.
func TestMapShape(t *testing.T) {
	r := NewR(99)
	for i := 0; i < 100; i++ {
		m := r.Map(2, 3, 2)
		if len(m) < 1 || len(m) > 3 {
			t.Fatalf("iter %d: root has %d keys, want [1,3]", i, len(m))
		}
	}
}
