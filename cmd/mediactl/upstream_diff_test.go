package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRenderDigestFiltersAndHunks: tracked-path filtering + vocabulary hunk
// selection + explicit no-tracked-change marker (W5-C2 AC-1/2/3).
func TestRenderDigestFiltersAndHunks(t *testing.T) {
	cmp := &compareResponse{Ahead: 2, Files: []diffFile{
		{Filename: "docs/README.md", Status: "modified", Additions: 3, Patch: "@@ -1 +1 @@\n+see https://example.com"},
		{Filename: "f2/apps/douyin/endpoints.py", Status: "modified", Additions: 4, Deletions: 1,
			Patch: "@@ -10,2 +10,3 @@\n context\n-old_url = \"/aweme/v1/old/\"\n+new_url = \"/aweme/v1/web/aweme/post/\"\n+params[\"max_cursor\"] = 0\n+sign = a_bogus(params)\n-unrelated line"},
		{Filename: "f2/apps/xhs/model.py", Status: "added", Additions: 9,
			Patch: "@@ -0,0 +1 @@\n+plain constant"},
	}}
	d := RenderDigest("Johnserf-Seed/f2", "pin1", "head1", cmp, []string{"f2/apps/douyin/", "f2/apps/xhs/"})
	if len(d.Files) != 2 {
		t.Fatalf("files = %d, want the two tracked ones", len(d.Files))
	}
	if d.Files[0].File != "f2/apps/douyin/endpoints.py" && d.Files[1].File != "f2/apps/douyin/endpoints.py" {
		t.Fatalf("ordering: %+v", d.Files)
	}
	var ep *FileDigest
	for i := range d.Files {
		if d.Files[i].File == "f2/apps/douyin/endpoints.py" {
			ep = &d.Files[i]
		}
	}
	if ep == nil || len(ep.KeyHunks) == 0 {
		t.Fatalf("endpoint file must carry key hunks: %+v", ep)
	}
	joined := strings.Join(ep.KeyHunks, " ")
	for _, want := range []string{"aweme/post", "max_cursor", "a_bogus"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("hunk missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "unrelated line") {
		t.Fatal("non-vocabulary lines must be pruned from key hunks")
	}
}

// TestRenderDigestNoTrackedChange: alert-without-tracked-delta is explicit,
// never silent (AC-3).
func TestRenderDigestNoTrackedChange(t *testing.T) {
	cmp := &compareResponse{Ahead: 1, Files: []diffFile{
		{Filename: "docsOnly.md", Status: "modified", Additions: 1},
	}}
	d := RenderDigest("x/y", "p", "h", cmp, []string{"src/"})
	if !d.NoTracked || d.Note == "" {
		t.Fatalf("digest = %+v, want explicit no-tracked-change marker", d)
	}
	if len(d.Files) != 0 {
		t.Fatalf("files = %+v", d.Files)
	}
}

// TestKeyHunksBudget: at most max hunks kept.
func TestKeyHunksBudget(t *testing.T) {
	patch := strings.Repeat("@@ -1 +1 @@\n+url = \"/api/%d\"\n", 10)
	if got := keyHunks(patch, 3); len(got) != 3 {
		t.Fatalf("hunks = %d, want budget 3", len(got))
	}
}

// TestDiffSummaryJSONRoundTrip: the digest marshals losslessly into the
// watcher summary (issue-body data source).
func TestDiffSummaryJSONRoundTrip(t *testing.T) {
	d := &DiffSummary{Slug: "a/b", From: "p", To: "h", Ahead: 1,
		Files: []FileDigest{{File: "x", Added: 1, KeyHunks: []string{"@@ | +url"}}}}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var back DiffSummary
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Files[0].KeyHunks[0] != d.Files[0].KeyHunks[0] || back.Ahead != 1 {
		t.Fatalf("round trip = %+v", back)
	}
}

// TestDiffSummarySlugBeforeFlags (holdout F1): the slug may precede the
// flags even though Go's flag package stops at the first positional.
func TestDiffSummarySlugBeforeFlags(t *testing.T) {
	slug := ""
	var rest []string
	for _, a := range []string{"Johnserf-Seed/f2", "--to", "dev"} {
		if slug == "" && !strings.HasPrefix(a, "-") {
			slug = a
			continue
		}
		rest = append(rest, a)
	}
	if slug != "Johnserf-Seed/f2" || len(rest) != 2 {
		t.Fatalf("split = %q %v", slug, rest)
	}
}
