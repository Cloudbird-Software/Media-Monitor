package collect

import "testing"

// TestResolveSegsKeyIndex: the "key[index]" path form must descend the key
// before indexing (report G4: "avatar_thumb.url_list[0]" used to miss on the
// author map because the index branch never descended the key). Negative
// indexes count from the end.
func TestResolveSegsKeyIndex(t *testing.T) {
	rec := map[string]any{
		"user": map[string]any{
			"avatar_thumb": map[string]any{
				"url_list": []any{"a0", "a1", "a2"},
			},
		},
		"video": map[string]any{
			"cover": map[string]any{"url_list": []any{"c0", "c1"}},
		},
	}
	segs, err := parseRel("user.avatar_thumb.url_list[0]")
	if err != nil {
		t.Fatalf("parseRel: %v", err)
	}
	if v := resolveSegs(rec, segs); v != "a0" {
		t.Fatalf("resolveSegs(url_list[0]) = %v, want a0", v)
	}

	segsNeg, err := parseRel("user.avatar_thumb.url_list[-1]")
	if err != nil {
		t.Fatalf("parseRel(-1): %v", err)
	}
	if v := resolveSegs(rec, segsNeg); v != "a2" {
		t.Fatalf("resolveSegs(url_list[-1]) = %v, want a2", v)
	}

	// The default cover_url candidate family ("video.cover.url_list[0]")
	// resolves too.
	segsCover, err := parseRel("video.cover.url_list[0]")
	if err != nil {
		t.Fatalf("parseRel(cover): %v", err)
	}
	if v := resolveSegs(rec, segsCover); v != "c0" {
		t.Fatalf("resolveSegs(cover.url_list[0]) = %v, want c0", v)
	}

	// Out-of-range and missing-key index forms stay misses (nil, no panic).
	for _, raw := range []string{"user.avatar_thumb.url_list[3]", "user.avatar_thumb.url_list[-4]", "user.missing[0]"} {
		s, err := parseRel(raw)
		if err != nil {
			t.Fatalf("parseRel(%q): %v", raw, err)
		}
		if v := resolveSegs(rec, s); v != nil {
			t.Fatalf("resolveSegs(%q) = %v, want nil", raw, v)
		}
	}

	// Plain keys and wildcards keep their old semantics.
	if s, _ := parseRel("user.nickname"); resolveSegs(map[string]any{"user": map[string]any{"nickname": "n"}}, s) != "n" {
		t.Fatal("plain key resolution regressed")
	}
	if s, _ := parseRel("user.avatar_thumb.url_list[]"); resolveSegs(rec, s) != "a0" {
		t.Fatal("wildcard [] resolution regressed")
	}
}

// TestParseRelDistinctForms: "a[0]", "a[]" and "a" parse into distinguishable
// segment shapes (indexed vs wildcard vs plain).
func TestParseRelDistinctForms(t *testing.T) {
	idx, err := parseRel("a[0]")
	if err != nil || len(idx) != 1 || !idx[0].indexed || idx[0].index != 0 || idx[0].key != "a" {
		t.Fatalf("parseRel(a[0]) = %+v err=%v", idx, err)
	}
	neg, err := parseRel("a[-1]")
	if err != nil || len(neg) != 1 || !neg[0].indexed || neg[0].index != -1 {
		t.Fatalf("parseRel(a[-1]) = %+v err=%v", neg, err)
	}
	wild, err := parseRel("a[]")
	if err != nil || len(wild) != 2 || wild[0].indexed || wild[0].index != -1 || !wild[1].star {
		t.Fatalf("parseRel(a[]) = %+v err=%v", wild, err)
	}
	plain, err := parseRel("a")
	if err != nil || len(plain) != 1 || plain[0].indexed || plain[0].index != -1 || plain[0].key != "a" {
		t.Fatalf("parseRel(a) = %+v err=%v", plain, err)
	}
}
