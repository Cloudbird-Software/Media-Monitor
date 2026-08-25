// Package contracts — JSONPath-lite walker used by contract binding and
// drift detection. Grammar: "$.a.b[0].c" — root "$", dot segments, integer
// indexes; the token "*" matches any object value or array element.
package contracts

import (
	"fmt"
	"strconv"
	"strings"
)

// Path is a parsed JSONPath-lite expression.
type Path struct {
	segs []seg
	raw  string
}

type seg struct {
	key   string
	index int
	star  bool // "*"
}

// ParsePath parses "$.a.b[3].*".
func ParsePath(raw string) (Path, error) {
	p := Path{raw: raw}
	if raw == "" {
		return p, fmt.Errorf("empty path")
	}
	body := raw
	if strings.HasPrefix(body, "$") {
		body = body[1:]
	}
	if body == "" {
		return p, nil // "$" = whole document
	}
	if !strings.HasPrefix(body, ".") {
		return p, fmt.Errorf("path %q: expected '$.' prefix", raw)
	}
	body = body[1:]
	for _, part := range strings.Split(body, ".") {
		if part == "" {
			return p, fmt.Errorf("path %q: empty segment", raw)
		}
		if part == "*" {
			p.segs = append(p.segs, seg{star: true})
			continue
		}
		key := part
		idx := -1
		if i := strings.IndexByte(part, '['); i >= 0 && strings.HasSuffix(part, "]") {
			key = part[:i]
			inner := part[i+1 : len(part)-1]
			// Empty or "*" index ("x[]", "x[*]") is treated as the wildcard
			// "x" + "*" — selects every element of the array at "x".
			if inner == "" || inner == "*" {
				p.segs = append(p.segs, seg{key: key, index: -1})
				p.segs = append(p.segs, seg{star: true})
				continue
			}
			n, err := strconv.Atoi(inner)
			if err != nil {
				return p, fmt.Errorf("path %q: bad index %q", raw, part)
			}
			idx = n
		}
		p.segs = append(p.segs, seg{key: key, index: idx})
	}
	return p, nil
}

func (p Path) String() string { return p.raw }

type value = any

// Select returns all values reachable at path from doc.
func (p Path) Select(doc any) []any {
	if len(p.segs) == 0 {
		return []any{doc}
	}
	cur := []any{doc}
	for _, s := range p.segs {
		var next []any
		for _, v := range cur {
			next = append(next, s.apply(v)...)
		}
		cur = next
		if len(cur) == 0 {
			return nil
		}
	}
	return cur
}

func (s seg) apply(v any) []any {
	var out []any
	walk := func(x any) {
		if s.star {
			switch t := x.(type) {
			case []any:
				out = append(out, t...)
			case map[string]any:
				for _, vv := range t {
					out = append(out, vv)
				}
			}
			return
		}
		switch t := x.(type) {
		case []any:
			if s.index >= 0 && s.index < len(t) {
				out = append(out, t[s.index])
			} else if s.index < 0 && s.key != "" {
				// key against array: collect matching object fields
				for _, e := range t {
					if m, ok := e.(map[string]any); ok {
						if vv, ok := m[s.key]; ok {
							out = append(out, vv)
						}
					}
				}
			}
		case map[string]any:
			if s.index >= 0 {
				// Key + index on an object: "a[1]" resolves key "a" then
				// indexes into the resulting array.
				if s.key != "" {
					if sub, ok := t[s.key]; ok {
						if arr, ok := sub.([]any); ok && s.index < len(arr) {
							out = append(out, arr[s.index])
						}
					}
				}
				return
			}
			if vv, ok := t[s.key]; ok {
				out = append(out, vv)
			}
		}
	}
	// Select() already iterates over every candidate value in cur and calls
	// apply once per candidate, so the segment is applied to the single
	// value v here. (The original `for _, v := range v` did not compile:
	// ranging over a value of interface type is not allowed.)
	walk(v)
	return out
}

// First returns the first selected value (nil when missing).
func (p Path) First(doc any) any {
	vs := p.Select(doc)
	if len(vs) == 0 {
		return nil
	}
	return vs[0]
}

// MustPath parses or panics — for tests and static initializers only.
func MustPath(raw string) Path {
	p, err := ParsePath(raw)
	if err != nil {
		panic(err)
	}
	return p
}
