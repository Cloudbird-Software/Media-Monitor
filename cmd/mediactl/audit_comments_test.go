package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fullAuthor returns a user map with all twelve mandated fields bound rich.
func fullAuthor(uid string) map[string]any {
	return map[string]any{
		"uid": uid, "sec_uid": "MS4wLjABAAAA-x", "short_id": "1000" + uid,
		"nickname": "用户" + uid, "avatar_url": "https://example.invalid/" + uid + ".jpg",
		"signature": "个签", "ip_label": "陕西", "gender": json.Number("2"),
		"follower_count": json.Number("1200"), "following_count": json.Number("88"),
		"aweme_count": json.Number("45"), "total_favorited": json.Number("99000"),
	}
}

func writeCommentsStore(t *testing.T, rows ...map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "comments.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	return dir
}

// TestFieldCompleteKinds: the documented counting rule per field kind.
func TestFieldCompleteKinds(t *testing.T) {
	cases := []struct {
		field string
		v     any
		want  bool
	}{
		{"uid", "2001", true},
		{"uid", "", false},
		{"uid", nil, false},            // absent key
		{"ip_label", "   ", false},     // whitespace-only is not data
		{"sec_uid", float64(5), false}, // wrong type
		{"gender", json.Number("2"), true},
		{"gender", json.Number("1"), true},
		{"gender", json.Number("0"), false}, // unknown ≠ present
		{"gender", "female", false},
		{"follower_count", json.Number("0"), true}, // explicit zero binds
		{"follower_count", json.Number("-3"), false},
		{"total_favorited", json.Number("12.5"), false}, // counters are integers
		{"aweme_count", "45", false},                    // must be a number
	}
	for _, tc := range cases {
		af := auditField{Name: tc.field, Kind: auditKindOf(tc.field)}
		if got := fieldComplete(af, tc.v); got != tc.want {
			t.Fatalf("fieldComplete(%s,%v)=%v want %v", tc.field, tc.v, got, tc.want)
		}
	}
	if len(auditFields) != 12 {
		t.Fatalf("audit fields = %d, want exactly 12", len(auditFields))
	}
}

func auditKindOf(field string) string {
	for _, af := range auditFields {
		if af.Name == field {
			return af.Kind
		}
	}
	return ""
}

// TestScanCommentsAuditRatios: golden author vs sparse author vs row
// without a user object — each of the twelve fields gets its honest share.
func TestScanCommentsAuditRatios(t *testing.T) {
	sparse := commentRow("c-2", "缺字段评论", func() map[string]any {
		u := fullAuthor("2002")
		delete(u, "nickname") // absent
		u["signature"] = ""   // empty
		u["gender"] = json.Number("0")
		return u
	}())
	dir := writeCommentsStore(t,
		commentRow("c-1", "全字段", fullAuthor("2001")),
		sparse,
		map[string]any{"cid": "c-3", "text": "无作者行"}, // no user object
	)
	res, err := scanCommentsAudit(dir, "comments")
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsScanned != 3 || res.Authors != 3 {
		t.Fatalf("scanned=%d authors=%d want 3/3", res.RowsScanned, res.Authors)
	}
	nick := pctFor(res, "nickname")
	if nick.Complete != 1 || nick.Total != 3 {
		t.Fatalf("nickname %d/%d want 1/3 (absent key counts incomplete)", nick.Complete, nick.Total)
	}
	gen := pctFor(res, "gender")
	if gen.Complete != 1 {
		t.Fatalf("gender complete=%d want 1 (unknown 0 and no-user count incomplete)", gen.Complete)
	}
	uid := pctFor(res, "uid")
	if uid.Complete != 2 {
		t.Fatalf("uid complete=%d want 2 (no-user row misses it)", uid.Complete)
	}
	if res.Pass {
		t.Fatalf("overall %.1f%% must not pass at floor %.1f%% with these gaps",
			res.OverallPct, res.MinPct)
	}

	// A fully-bound single author passes the 90% AC-19 floor.
	goldenDir := writeCommentsStore(t, commentRow("g-1", "好", fullAuthor("9001")))
	res2, err := scanCommentsAudit(goldenDir, "comments")
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Pass || res2.OverallPct < defaultMinCompleteness {
		t.Fatalf("golden author must pass: overall=%.1f pass=%v", res2.OverallPct, res2.Pass)
	}
}

// TestScanCommentsAuditFailClosed: a missing collection or a malformed row
// must surface explicit errors — never a vacuous pass.
func TestScanCommentsAuditFailClosed(t *testing.T) {
	if _, err := scanCommentsAudit(t.TempDir(), "comments"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing collection must fail closed, got %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "comments.jsonl"), []byte("{bad json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := scanCommentsAudit(dir, "comments")
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("malformed row must name its line, got %v", err)
	}
	if _, err := scanCommentsAudit("", ""); err == nil {
		t.Fatal("empty store dir must error")
	}
}

// TestFieldAuditCountsTwelveFieldsExactly: the AC-19 ledger covers exactly
// the twelve mandated names from model.go's completeness contract.
func TestFieldAuditCountsTwelveFieldsExactly(t *testing.T) {
	want := []string{"uid", "sec_uid", "short_id", "nickname", "avatar_url",
		"signature", "ip_label", "gender", "follower_count", "following_count",
		"aweme_count", "total_favorited"}
	got := make([]string, 0, len(auditFields))
	for _, f := range auditFields {
		got = append(got, f.Name)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("field[%d]=%q want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
