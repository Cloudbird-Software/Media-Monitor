package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// defaultRegistryPath is the upstream ecosystem registry everyone reads.
const defaultRegistryPath = "upstream/registry.json"

// githubAPIBase is where the GitHub REST API lives; tests redirect it to an
// httptest server so scans never touch the real network.
var githubAPIBase = "https://api.github.com"

// rateLimitBackoff is the wait between a 429 answer and the single retry;
// tests shorten it.
var rateLimitBackoff = 60 * time.Second

// upstreamEntry is one tracked upstream project (hand-written mirror of
// upstream/registry.json; pin and notes are ignored by the scanner).
type upstreamEntry struct {
	Slug    string `json:"slug"`
	Role    string `json:"role"`
	License struct {
		SPDX    string `json:"spdx"`
		Verdict string `json:"verdict"`
	} `json:"license"`
	Pin struct {
		Type string `json:"type"`
		Ref  string `json:"ref"`
	} `json:"pin"`
	TrackedPaths []string `json:"tracked_paths"`
}

// loadUpstreamRegistry reads the on-disk registry.
func loadUpstreamRegistry() (*upstreamRegistry, error) {
	raw, err := os.ReadFile(defaultRegistryPath)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", defaultRegistryPath, err)
	}
	var reg upstreamRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("registry %s: %w", defaultRegistryPath, err)
	}
	return &reg, nil
}

// upstreamRegistry is the top-level registry document.
type upstreamRegistry struct {
	Version int             `json:"version"`
	Entries []upstreamEntry `json:"entries"`
}

// upstreamHit reports one entry with matching upstream activity.
type upstreamHit struct {
	Slug         string       `json:"slug"`
	Role         string       `json:"role"`
	MatchedFiles []string     `json:"matched_files"`
	FirstSHA     string       `json:"first_sha"`
	Title        string       `json:"title"`
	Diff         *DiffSummary `json:"diff_summary,omitempty"` // pin..first-matching-sha digest (W5-C2)
}

// upstreamError reports one entry that could not be scanned.
type upstreamError struct {
	Slug  string `json:"slug"`
	Error string `json:"error"`
}

// upstreamSummary is the machine-readable scan result written to --out.
type upstreamSummary struct {
	ScanStarted  string          `json:"scan_started"`
	WindowHours  int             `json:"window_hours"`
	TotalEntries int             `json:"total_entries"`
	Scanned      int             `json:"scanned"`
	Hits         []upstreamHit   `json:"hits"`
	Errors       []upstreamError `json:"errors"`
}

func cmdUpstream(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use: upstream scan | upstream diff-summary <slug> | upstream swap-test <slug>")
	}
	switch args[0] {
	case "scan":
		return upstreamScan(args[1:])
	case "swap-test":
		return upstreamSwapTest(args[1:])
	case "diff-summary":
		return upstreamDiffSummary(args[1:])
	default:
		return fmt.Errorf("unknown upstream subcommand %q", args[0])
	}
}

// upstreamScan implements `upstream scan`: poll GitHub commit activity of
// every registry entry within the window, match filenames against the entry
// tracked_paths, write the JSON summary and print a digest.
func upstreamScan(args []string) error {
	fs := flag.NewFlagSet("upstream scan", flag.ExitOnError)
	window := fs.Int("window-hours", 24, "scan window in hours")
	out := fs.String("out", "adapt/reports/upstream-summary.json", "JSON summary output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *window <= 0 {
		return fmt.Errorf("--window-hours must be > 0")
	}
	raw, err := os.ReadFile(defaultRegistryPath)
	if err != nil {
		return fmt.Errorf("registry %s: %w", defaultRegistryPath, err)
	}
	var reg upstreamRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return fmt.Errorf("registry %s: %w", defaultRegistryPath, err)
	}
	ctx := context.Background()
	sum := runUpstreamScan(ctx, reg.Entries, *window, time.Now().Add(-time.Duration(*window)*time.Hour), os.Getenv("GITHUB_TOKEN"))
	if err := writeUpstreamSummary(*out, sum); err != nil {
		return err
	}
	printUpstreamSummary(sum, *out)
	return nil
}

// runUpstreamScan polls every entry. Individual failures land in the
// summary's Errors and never fail the whole run.
func runUpstreamScan(ctx context.Context, entries []upstreamEntry, windowHours int, since time.Time, token string) upstreamSummary {
	sum := upstreamSummary{
		ScanStarted:  time.Now().UTC().Format(time.RFC3339),
		WindowHours:  windowHours,
		TotalEntries: len(entries),
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	sinceStr := since.UTC().Format(time.RFC3339)
	for _, e := range entries {
		hit, entryErr := scanEntry(ctx, hc, e, sinceStr, token)
		if entryErr != nil {
			sum.Errors = append(sum.Errors, upstreamError{Slug: e.Slug, Error: entryErr.Error()})
			continue
		}
		sum.Scanned++
		if hit != nil {
			// W5-C2: attach the pin..first-matching-sha digest so the
			// alert body carries the file-level diff summary directly.
			if e.Pin.Ref != "" && hit.FirstSHA != "" && e.Pin.Ref != hit.FirstSHA {
				if cmp, cerr := fetchCompare(ctx, hc, e.Slug, e.Pin.Ref, hit.FirstSHA, token); cerr == nil {
					hit.Diff = RenderDigest(e.Slug, e.Pin.Ref, hit.FirstSHA, cmp, e.TrackedPaths)
				} else {
					hit.Diff = &DiffSummary{Slug: e.Slug, From: e.Pin.Ref, To: hit.FirstSHA,
						Note: "digest unavailable: " + cerr.Error()}
				}
			}
			sum.Hits = append(sum.Hits, *hit)
		}
	}
	return sum
}

// scanEntry polls the commit list of one entry and, for every commit, its
// changed files, matching against the tracked paths. A nil hit with nil
// error means "scanned, no matches".
func scanEntry(ctx context.Context, hc *http.Client, e upstreamEntry, sinceStr, token string) (*upstreamHit, error) {
	commits, err := fetchCommits(ctx, hc, e.Slug, sinceStr, token)
	if err != nil {
		return nil, err
	}
	hit := &upstreamHit{Slug: e.Slug, Role: e.Role}
	seen := map[string]bool{}
	var firstErr error
	for _, c := range commits {
		var files []string
		if len(e.TrackedPaths) > 0 {
			files, err = fetchCommitFiles(ctx, hc, e.Slug, c.SHA, token)
			if err != nil {
				firstErr = err
				break
			}
		}
		for _, f := range files {
			if !matchesTracked(f, e.TrackedPaths) {
				continue
			}
			if hit.FirstSHA == "" {
				hit.FirstSHA = c.SHA
				hit.Title = firstLine(c.Commit.Message)
			}
			if !seen[f] {
				seen[f] = true
				hit.MatchedFiles = append(hit.MatchedFiles, f)
			}
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if hit.FirstSHA == "" {
		return nil, nil
	}
	sort.Strings(hit.MatchedFiles)
	return hit, nil
}

// ghCommit is the subset of the commits list API we consume.
type ghCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// fetchCommits lists the commits of a repository within the since window
// (per_page=20, same as the watcher).
func fetchCommits(ctx context.Context, hc *http.Client, slug, since, token string) ([]ghCommit, error) {
	u := fmt.Sprintf("%s/repos/%s/commits?since=%s&per_page=20", githubAPIBase, slug, url.QueryEscape(since))
	raw, err := ghGet(ctx, hc, u, token)
	if err != nil {
		return nil, fmt.Errorf("commits for %s: %w", slug, err)
	}
	var out []ghCommit
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("commits for %s: decode: %w", slug, err)
	}
	return out, nil
}

// fetchCommitFiles resolves the changed filenames of one commit.
func fetchCommitFiles(ctx context.Context, hc *http.Client, slug, sha, token string) ([]string, error) {
	u := fmt.Sprintf("%s/repos/%s/commits/%s", githubAPIBase, slug, url.PathEscape(sha))
	raw, err := ghGet(ctx, hc, u, token)
	if err != nil {
		return nil, fmt.Errorf("commit files for %s@%s: %w", slug, shortSHA(sha), err)
	}
	var doc struct {
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("commit files for %s@%s: decode: %w", slug, shortSHA(sha), err)
	}
	out := make([]string, 0, len(doc.Files))
	for _, f := range doc.Files {
		out = append(out, f.Filename)
	}
	return out, nil
}

// ghGet performs one authenticated GET, retrying once after
// rateLimitBackoff on 429 (rate limit exhaustion) and failing closed on
// every other non-2xx status.
func ghGet(ctx context.Context, hc *http.Client, u, token string) ([]byte, error) {
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(rateLimitBackoff):
			}
		}
		body, status, err := ghGetOnce(ctx, hc, u, token)
		if err != nil {
			return nil, err
		}
		switch {
		case status == http.StatusTooManyRequests:
			if attempt == 2 {
				return nil, fmt.Errorf("github api: rate limited (429) after retry")
			}
		case status >= 400:
			snippet := strings.TrimSpace(string(body))
			if len(snippet) > 160 {
				snippet = snippet[:160]
			}
			return nil, fmt.Errorf("github api: status %d: %s", status, snippet)
		default:
			return body, nil
		}
	}
	return nil, fmt.Errorf("github api: unreachable")
}

func ghGetOnce(ctx context.Context, hc *http.Client, u, token string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}

// matchesTracked applies an entry's tracked_paths to one filename: patterns
// without '*' are substring matches, patterns with '*' are matched
// part-by-part in order ("*.py" behaves as a suffix, "*webmssdk*" as a
// substring, "app/src/*" as a prefix).
func matchesTracked(filename string, patterns []string) bool {
	for _, p := range patterns {
		if matchPattern(filename, strings.TrimSpace(p)) {
			return true
		}
	}
	return false
}

func matchPattern(filename, pat string) bool {
	if pat == "" {
		return false
	}
	if !strings.Contains(pat, "*") {
		return strings.Contains(filename, pat)
	}
	rest := filename
	for _, part := range strings.Split(pat, "*") {
		if part == "" {
			continue
		}
		i := strings.Index(rest, part)
		if i < 0 {
			return false
		}
		rest = rest[i+len(part):]
	}
	// A literal tail ("*.py") must consume the filename to its end; an open
	// tail ("app/src/*") absorbs everything remaining.
	if !strings.HasSuffix(pat, "*") {
		return rest == ""
	}
	return true
}

// writeUpstreamSummary persists the scan result (MkdirAll for --out parents).
func writeUpstreamSummary(out string, sum upstreamSummary) error {
	raw, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if dir := filepath.Dir(out); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("summary dir: %w", err)
		}
	}
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		return fmt.Errorf("summary %s: %w", out, err)
	}
	return nil
}

// printUpstreamSummary renders the human digest on stdout.
func printUpstreamSummary(sum upstreamSummary, out string) {
	fmt.Printf("upstream scan: window %dh, %d entries, %d scanned, %d hit(s), %d error(s)\n",
		sum.WindowHours, sum.TotalEntries, sum.Scanned, len(sum.Hits), len(sum.Errors))
	for _, h := range sum.Hits {
		fmt.Printf("  - %s (%s): %d matched file(s), first commit %s %q\n",
			h.Slug, h.Role, len(h.MatchedFiles), shortSHA(h.FirstSHA), h.Title)
	}
	for _, e := range sum.Errors {
		fmt.Printf("  - %s: %s\n", e.Slug, e.Error)
	}
	if out != "" {
		fmt.Printf("summary written to %s\n", out)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
