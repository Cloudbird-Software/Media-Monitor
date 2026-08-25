package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ghMock records what the fake GitHub API saw (handlers run on server
// goroutines, so access is mutex-guarded).
type ghMock struct {
	mu        sync.Mutex
	auth      string
	lastSince string
}

// newGitHubMock serves /repos/{owner}/{repo}/commits and
// /repos/{owner}/{repo}/commits/{sha} with the given commitWorld.
func newGitHubMock(t *testing.T, m *ghMock, world map[string]map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.auth = r.Header.Get("Authorization")
		m.mu.Unlock()
		if r.URL.Path == "/repos/A/B/commits" {
			m.mu.Lock()
			m.lastSince = r.URL.Query().Get("since")
			m.mu.Unlock()
			if r.URL.Query().Get("per_page") != "20" {
				t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
			}
		}
		files, ok := world[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, files["body"])
	}))
	t.Cleanup(srv.Close)
	githubAPIBase = srv.URL
	return srv
}

func ghEntry(slug, role string, tracked ...string) upstreamEntry {
	return upstreamEntry{Slug: slug, Role: role, TrackedPaths: tracked}
}

func TestUpstreamScanHitsAndAuth(t *testing.T) {
	m := &ghMock{}
	world := map[string]map[string]string{
		"/repos/A/B/commits":            {"body": `[{"sha":"aaa1111111","commit":{"message":"first commit\n\nbody"}},{"sha":"aaa2222222","commit":{"message":"second"}}]`},
		"/repos/A/B/commits/aaa1111111": {"body": `{"sha":"aaa1111","files":[{"filename":"src/main.go"},{"filename":"docs/readme.md"}]}`},
		"/repos/A/B/commits/aaa2222222": {"body": `{"sha":"aaa2222","files":[{"filename":"src/other.go"}]}`},
		"/repos/C/D/commits":            {"body": `[{"sha":"ccc1111111","commit":{"message":"unrelated"}}]`},
		"/repos/C/D/commits/ccc1111111": {"body": `{"sha":"ccc1111","files":[{"filename":"README.md"}]}`},
	}
	if world["/repos/A/B/commits/aaa1111111"] == nil {
		t.Fatal("bad fixture")
	}
	newGitHubMock(t, m, world)
	entries := []upstreamEntry{
		ghEntry("A/B", "collector", "src/"),
		ghEntry("C/D", "vision", "specs/"),
	}
	sum := runUpstreamScan(context.Background(), entries, 24, time.Now().Add(-24*time.Hour), "tok-abc")

	if sum.TotalEntries != 2 || sum.Scanned != 2 || len(sum.Hits) != 1 || len(sum.Errors) != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	hit := sum.Hits[0]
	if hit.Slug != "A/B" || hit.Role != "collector" {
		t.Fatalf("hit = %+v", hit)
	}
	if len(hit.MatchedFiles) != 2 || hit.MatchedFiles[0] != "src/main.go" || hit.MatchedFiles[1] != "src/other.go" {
		t.Fatalf("matched files = %v", hit.MatchedFiles)
	}
	if hit.FirstSHA != "aaa1111111" || hit.Title != "first commit" {
		t.Fatalf("first = %s %q", hit.FirstSHA, hit.Title)
	}
	if sum.ScanStarted == "" || sum.WindowHours != 24 {
		t.Fatalf("scan_started / window = %+v", sum)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.auth != "Bearer tok-abc" {
		t.Fatalf("Authorization = %q", m.auth)
	}
	if m.lastSince == "" {
		t.Fatal("since param missing")
	}
}

func TestUpstreamScanGlobPatterns(t *testing.T) {
	if !matchesTracked("f2/apps/douyin/collector.py", []string{"f2/apps/douyin/"}) {
		t.Fatal("substring path should match")
	}
	if !matchesTracked("webmssdk.js", []string{"*webmssdk*"}) {
		t.Fatal("contains-glob should match")
	}
	if !matchesTracked("crawlers/main.x", []string{"*.py", "crawlers/"}) {
		t.Fatal("suffix-glob should match via the other pattern")
	}
	if matchesTracked("doc.pyx", []string{"*.py"}) {
		t.Fatal("suffix-glob matched a non-suffix file")
	}
	if matchesTracked("README.md", []string{"src/"}) {
		t.Fatal("substring path matched a stray file")
	}
}

func TestUpstreamScan429RetryThenSuccess(t *testing.T) {
	old := rateLimitBackoff
	rateLimitBackoff = 20 * time.Millisecond
	defer func() { rateLimitBackoff = old }()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/A/B/commits":
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
				return
			}
			_, _ = io.WriteString(w, `[{"sha":"aaa1111111","commit":{"message":"back"},"files":[]}]`)
		case "/repos/A/B/commits/aaa1111111":
			_, _ = io.WriteString(w, `{"sha":"aaa1","files":[{"filename":"src/x.go"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	sum := runUpstreamScan(context.Background(), []upstreamEntry{ghEntry("A/B", "collector", "src/")}, 24, time.Now().Add(-24*time.Hour), "")
	if calls.Load() != 2 {
		t.Fatalf("commit API calls = %d, want 2 (429 + retry)", calls.Load())
	}
	if sum.Scanned != 1 || len(sum.Hits) != 1 || len(sum.Errors) != 0 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestUpstreamScanErrorsDoNotFailRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/E/F/commits":
			w.WriteHeader(http.StatusInternalServerError)
		case "/repos/G/H/commits":
			_, _ = io.WriteString(w, `not json`)
		case "/repos/A/B/commits":
			_, _ = io.WriteString(w, `[{"sha":"aaa1111111","commit":{"message":"m"}}]`)
		case "/repos/A/B/commits/aaa1111111":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	entries := []upstreamEntry{
		ghEntry("E/F", "collector", "src/"),
		ghEntry("G/H", "vision", "src/"),
		ghEntry("A/B", "collector", "src/"),
	}
	sum := runUpstreamScan(context.Background(), entries, 24, time.Now().Add(-24*time.Hour), "")
	if sum.Scanned != 0 || len(sum.Hits) != 0 || len(sum.Errors) != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	saw := map[string]bool{}
	for _, e := range sum.Errors {
		saw[e.Slug] = true
		if e.Error == "" {
			t.Fatalf("error without message: %+v", e)
		}
	}
	for _, slug := range []string{"E/F", "G/H", "A/B"} {
		if !saw[slug] {
			t.Fatalf("missing error for %s: %+v", slug, sum.Errors)
		}
	}
}

func TestUpstreamScanCLIWritesOutFile(t *testing.T) {
	m := &ghMock{}
	world := map[string]map[string]string{
		"/repos/A/B/commits":            {"body": `[{"sha":"aaa1111111","commit":{"message":"cli hit"}}]`},
		"/repos/A/B/commits/aaa1111111": {"body": `{"sha":"aaa1","files":[{"filename":"app/src/a.go"}]}`},
	}
	if world["/repos/A/B/commits/aaa1111111"] == nil {
		t.Fatal("bad fixture")
	}
	newGitHubMock(t, m, world)

	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "upstream"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := `{"version":1,"entries":[{"slug":"A/B","role":"collector","license":{"spdx":"Apache-2.0","verdict":"allowed"},"tracked_paths":["app/src/"]}]}`
	if err := os.WriteFile(filepath.Join(tmp, "upstream", "registry.json"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "reports", "sum.json")
	t.Chdir(tmp)

	t.Setenv("GITHUB_TOKEN", "")
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	cmdErr := cmdUpstream([]string{"scan", "--window-hours", "24", "--out", out})
	_ = w.Close()
	os.Stdout = oldStdout
	stdout, _ := io.ReadAll(r)
	if cmdErr != nil {
		t.Fatalf("scan: %v", cmdErr)
	}
	if !strings.Contains(string(stdout), "1 hit(s)") || !strings.Contains(string(stdout), "aaa1111") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(string(stdout), "summary written to "+out) {
		t.Fatalf("stdout = %q", stdout)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var sum upstreamSummary
	if err := json.Unmarshal(raw, &sum); err != nil {
		t.Fatalf("out file is not JSON: %v", err)
	}
	if sum.TotalEntries != 1 || sum.Scanned != 1 || len(sum.Hits) != 1 || len(sum.Errors) != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Hits[0].FirstSHA != "aaa1111111" || sum.Hits[0].Title != "cli hit" {
		t.Fatalf("hit = %+v", sum.Hits[0])
	}
	// No token configured: the API must have been called unauthenticated.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.auth != "" {
		t.Fatalf("Authorization = %q, want unauthenticated", m.auth)
	}
}

func TestUpstreamScanCLIValidation(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	if err := cmdUpstream([]string{"nope"}); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
	if err := cmdUpstream([]string{"scan", "--window-hours", "0"}); err == nil {
		t.Fatal("zero window accepted")
	}
	// Missing registry file.
	tmp := t.TempDir()
	t.Chdir(tmp)
	if err := cmdUpstream([]string{"scan", "--window-hours", "24", "--out", filepath.Join(tmp, "s.json")}); err == nil {
		t.Fatal("missing registry accepted")
	}
}
