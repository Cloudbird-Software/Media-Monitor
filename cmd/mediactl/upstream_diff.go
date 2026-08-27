// upstream diff-summary (IR-MM-0001 AC-12, W5-C2): when the watcher fires,
// the alert must carry a file-level diff digest of the tracked paths so an
// agent can locate the contract change without re-deriving it. The digest
// uses the GitHub compare API (pin...latest) — zero clone, satisfying the
// runner's shallow-pull budget by construction.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// diffFile is one changed tracked file.
type diffFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch,omitempty"` // full patch (API-provided)
}

// compareResponse is the subset of the compare API payload we consume.
type compareResponse struct {
	Status string     `json:"status"`
	Ahead  int        `json:"ahead_by"`
	Files  []diffFile `json:"files"`
}

// FileDigest is the rendered digest entry for one tracked file.
type FileDigest struct {
	File     string   `json:"file"`
	Status   string   `json:"status"`
	Added    int      `json:"added"`
	Deleted  int      `json:"deleted"`
	KeyHunks []string `json:"key_hunks"` // endpoint/param/signature-relevant hunks
}

// DiffSummary is the per-hit digest attached to watcher alerts.
type DiffSummary struct {
	Slug      string       `json:"slug"`
	From      string       `json:"from"`
	To        string       `json:"to"`
	Ahead     int          `json:"ahead_by"`
	Files     []FileDigest `json:"files"`
	NoTracked bool         `json:"no_tracked_change"` // alert fired but no tracked_paths delta
	Note      string       `json:"note,omitempty"`
}

// keyLineRe selects diff lines worth surfacing (endpoint/param/signature
// vocabulary, per the card's "关键 hunk" requirement).
var keyLineRe = regexp.MustCompile(`(?i)(url|uri|endpoint|api/|aweme|sns/web|a_bogus|mstoken|x-bogus|x-s|x-t|sign|param|cursor|sec_user|user_id|version_code|license)`)

// RenderDigest filters compare files by tracked paths and renders hunks.
func RenderDigest(slug, from, to string, cmp *compareResponse, patterns []string) *DiffSummary {
	d := &DiffSummary{Slug: slug, From: from, To: to, Ahead: cmp.Ahead}
	for _, f := range cmp.Files {
		if !matchesTracked(f.Filename, patterns) {
			continue
		}
		fd := FileDigest{File: f.Filename, Status: f.Status, Added: f.Additions, Deleted: f.Deletions}
		fd.KeyHunks = keyHunks(f.Patch, 3)
		d.Files = append(d.Files, fd)
	}
	if len(d.Files) == 0 {
		d.NoTracked = true
		d.Note = "报警但无 tracked_paths 变更（非静默：显式标注）"
	}
	return d
}

// keyHunks extracts hunks (changed-line groups) containing vocabulary
// lines, at most max.
func keyHunks(patch string, max int) []string {
	if patch == "" {
		return nil
	}
	var out []string
	lines := strings.Split(patch, "\n")
	var cur []string
	hit := false
	flush := func() {
		if hit && len(cur) > 0 && len(out) < max {
			// keep it tight: hunk header + up to 6 vocabulary lines
			var keep []string
			header := ""
			if strings.HasPrefix(cur[0], "@@") {
				header = cur[0]
			}
			count := 0
			for _, l := range cur {
				if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
					if keyLineRe.MatchString(l) && count < 6 {
						keep = append(keep, l)
						count++
					}
				}
			}
			if len(keep) > 0 {
				out = append(out, header+" | "+strings.Join(keep, " ⏎ "))
			}
		}
		cur, hit = nil, false
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "@@") {
			flush()
			cur = []string{l}
			continue
		}
		if len(cur) == 0 {
			continue // preamble before first hunk
		}
		cur = append(cur, l)
		if keyLineRe.MatchString(l) && (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")) {
			hit = true
		}
	}
	flush()
	return out
}

// fetchCompare pulls pin..head via the compare API.
func fetchCompare(ctx context.Context, hc *http.Client, slug, base, head, token string) (*compareResponse, error) {
	raw, err := ghGet(ctx, hc, fmt.Sprintf("https://api.github.com/repos/%s/compare/%s...%s?per_page=100", slug, base, head), token)
	if err != nil {
		return nil, err
	}
	var cmp compareResponse
	if err := json.Unmarshal(raw, &cmp); err != nil {
		return nil, err
	}
	return &cmp, nil
}

// upstreamDiffSummary implements `upstream diff-summary <slug>`: render the
// pin..latest digest for one registry entry.
func upstreamDiffSummary(args []string) error {
	fs := flag.NewFlagSet("upstream diff-summary", flag.ExitOnError)
	to := fs.String("to", "", "head ref (default: the entry's default branch HEAD)")
	out := fs.String("out", "", "write the JSON digest to this file")
	// Accept `<slug> [flags]` as well as `[flags] <slug>` (holdout F1: Go's
	// flag package stops parsing at the first positional argument).
	slug := ""
	var rest []string
	for _, a := range args {
		if slug == "" && !strings.HasPrefix(a, "-") {
			slug = a
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: upstream diff-summary <slug> [--to <ref>] [--out FILE]")
	}
	if slug == "" {
		return fmt.Errorf("usage: upstream diff-summary <slug> [--to <ref>] [--out FILE] (slug required)")
	}
	reg, err := loadUpstreamRegistry()
	if err != nil {
		return err
	}
	var entry *upstreamEntry
	for i := range reg.Entries {
		if reg.Entries[i].Slug == slug {
			entry = &reg.Entries[i]
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("slug %q not in registry", slug)
	}
	from := entry.Pin.Ref
	if from == "" {
		return fmt.Errorf("entry %q has no pin to diff from", slug)
	}
	head := *to
	if head == "" {
		head = "HEAD"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hc := &http.Client{Timeout: 20 * time.Second}
	cmp, err := fetchCompare(ctx, hc, slug, from, head, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		return fmt.Errorf("compare %s (%s...%s): %w", slug, from, head, err)
	}
	d := RenderDigest(slug, from, head, cmp, entry.TrackedPaths)
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
	}
	fmt.Println(string(b))
	return nil
}

// sortDigestNames keeps deterministic file ordering for tests.
func sortDigestNames(d *DiffSummary) {
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].File < d.Files[j].File })
}
