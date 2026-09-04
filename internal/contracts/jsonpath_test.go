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

// TestParsePathKeyIndexOnMap: "key[0]" must descend the key and then index
// into the resulting array — the report-G4 shape ("avatar_thumb.url_list[0]"
// style). Negative indexes count from the end.
func TestParsePathKeyIndexOnMap(t *testing.T) {
	doc := map[string]any{
		"avatar_thumb": map[string]any{
			"url_list": []any{"u0", "u1", "u2"},
		},
	}
	p, err := ParsePath("$.avatar_thumb.url_list[0]")
	if err != nil {
		t.Fatalf("ParsePath: %v", err)
	}
	if v := p.First(doc); v != "u0" {
		t.Fatalf("First($.avatar_thumb.url_list[0]) = %v, want u0", v)
	}

	pn, err := ParsePath("$.avatar_thumb.url_list[-1]")
	if err != nil {
		t.Fatalf("ParsePath(-1): %v", err)
	}
	if v := pn.First(doc); v != "u2" {
		t.Fatalf("First($..url_list[-1]) = %v, want u2", v)
	}

	// Out-of-range negative index selects nothing (no panic).
	poob, _ := ParsePath("$.avatar_thumb.url_list[-4]")
	if vs := poob.Select(doc); len(vs) != 0 {
		t.Fatalf("Select(-4) = %v, want none", vs)
	}

	// A plain key path must NOT be treated as an index form (regression for
	// the parseRel/plain-key distinction).
	pp, _ := ParsePath("$.avatar_thumb.url_list")
	if v := pp.First(doc); v == nil {
		t.Fatal("First($.avatar_thumb.url_list) = nil, want the array")
	}
}
