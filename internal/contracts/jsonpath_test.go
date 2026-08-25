package contracts

import "testing"

// TestParsePathEmptyIndexWildcard: "x[]" and "x[*]" parse as "x" + "*" —
// they select every element of the array at "x".
func TestParsePathEmptyIndexWildcard(t *testing.T) {
	p, err := ParsePath("$.data[].id")
	if err != nil {
		t.Fatalf("ParsePath($.data[].id): %v", err)
	}
	doc := map[string]any{"data": []any{
		map[string]any{"id": "a"},
		map[string]any{"id": "b"},
	}}
	vs := p.Select(doc)
	if len(vs) != 2 || vs[0] != "a" || vs[1] != "b" {
		t.Fatalf("Select($.data[].id) = %v, want [a b]", vs)
	}

	p2, err := ParsePath("$.data[*].id")
	if err != nil {
		t.Fatalf("ParsePath($.data[*].id): %v", err)
	}
	vs = p2.Select(doc)
	if len(vs) != 2 || vs[0] != "a" || vs[1] != "b" {
		t.Fatalf("Select($.data[*].id) = %v, want [a b]", vs)
	}
}

// TestParsePathWildcardNestedIndex: wildcards combine with integer indexes on
// later segments.
func TestParsePathWildcardNestedIndex(t *testing.T) {
	p, err := ParsePath("$.comments[].user.uid")
	if err != nil {
		t.Fatalf("ParsePath: %v", err)
	}
	doc := map[string]any{"comments": []any{
		map[string]any{"user": map[string]any{"uid": "u1"}},
		map[string]any{"user": map[string]any{"uid": "u2"}},
	}}
	vs := p.Select(doc)
	if len(vs) != 2 || vs[0] != "u1" || vs[1] != "u2" {
		t.Fatalf("Select = %v, want [u1 u2]", vs)
	}

	pi, err := ParsePath("$.a[].b[1]")
	if err != nil {
		t.Fatalf("ParsePath: %v", err)
	}
	doc2 := map[string]any{"a": []any{
		map[string]any{"b": []any{"x0", "x1"}},
		map[string]any{"b": []any{"y0", "y1"}},
	}}
	vs = pi.Select(doc2)
	if len(vs) != 2 || vs[0] != "x1" || vs[1] != "y1" {
		t.Fatalf("Select = %v, want [x1 y1]", vs)
	}
}

// TestParsePathIntegerIndexUnchanged: existing numeric-index behavior and
// invalid index error text are preserved.
func TestParsePathIntegerIndexUnchanged(t *testing.T) {
	p, err := ParsePath("$.a[1].b")
	if err != nil {
		t.Fatalf("ParsePath: %v", err)
	}
	vs := p.Select(map[string]any{"a": []any{
		map[string]any{"b": "x"},
		map[string]any{"b": "y"},
	}})
	if len(vs) != 1 || vs[0] != "y" {
		t.Fatalf("Select = %v, want [y]", vs)
	}
	if _, err := ParsePath("$.a[zz]"); err == nil {
		t.Fatal("ParsePath($.a[zz]) succeeded, want error")
	}
}
