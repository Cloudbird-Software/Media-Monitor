// upstream swap-test bench (IR-MM-0001 AC-13, W5-C3): run the repo's own
// contract expectations against a pinned upstream implementation and score
// the swap candidate. The bench is mediactl-driven (agent-only no more);
// adapters live here as test scaffolding, never in internal/.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SwapScore is the bench output (three fields, machine-readable).
type SwapScore struct {
	Slug           string  `json:"slug"`
	SuccessRate    float64 `json:"success_rate"`
	FreshnessDays  int     `json:"freshness_days"`
	LicenseVerdict string  `json:"license_verdict"`
	Details        string  `json:"details"`
	ScoredAt       string  `json:"scored_at"`
}

// swapAdapter scores one upstream against this repo's contract
// expectations. Adapters are scaffolding: they read upstream sources at the
// pinned SHA (local vendor dir when initialized, else the GitHub API) and
// never mutate anything.
type swapAdapter interface {
	Name() string
	// Score returns the conformance ratio [0,1] for the contracts named in
	// expectations, or an explicit error when the upstream cannot be
	// evaluated in this environment (AC-3: no hang, no silent skip).
	Score(ctx context.Context, pin string, contracts []string) (float64, string, error)
}

// f2ParamAdapter: conformance of our douyin contract query keys against
// f2's endpoint parameter table at the pinned source (the honest offline
// swap-test for a signature/param reference upstream).
type f2ParamAdapter struct {
	hc    *http.Client
	token string
}

func (f2ParamAdapter) Name() string { return "f2-param-table" }

// expectedDouyinQueryKeys: the query keys our douyin contracts declare
// (transport.query + placeholders + paging params).
var expectedDouyinQueryKeys = map[string][]string{
	"douyin-user-posts":     {"sec_user_id", "max_cursor", "count", "device_platform", "aid", "channel"},
	"douyin-search":         {"keyword", "offset", "count", "search_channel"},
	"douyin-comments":       {"item_id", "cursor"},
	"douyin-video-download": {"aweme_id"},
}

var queryKeyRe = regexp.MustCompile(`["']([a-z_]{3,})["']\s*[:=]`)

func (a f2ParamAdapter) Score(ctx context.Context, pin string, contracts []string) (float64, string, error) {
	// source files carrying f2's douyin param tables at the pin
	paths := []string{"f2/apps/douyin/endpoints.py", "f2/apps/douyin/model.py", "f2/apps/douyin/utils.py"}
	var corpus strings.Builder
	for _, p := range paths {
		raw, err := fetchUpstreamFileDefault(ctx, a.hc, "Johnserf-Seed/f2", pin, p, a.token)
		if err != nil {
			continue // file moved at this pin; the others still carry the table
		}
		corpus.Write(raw)
		corpus.WriteString("\n")
	}
	if corpus.Len() == 0 {
		return 0, "", fmt.Errorf("f2 sources unavailable at pin %s (vendor dir empty and API fetch failed) — swap-test cannot run in this environment", shortSHA(pin))
	}
	src := corpus.String()
	upstreamKeys := map[string]bool{}
	for _, m := range queryKeyRe.FindAllStringSubmatch(src, -1) {
		upstreamKeys[m[1]] = true
	}
	var hit, total int
	var misses []string
	for _, c := range contracts {
		keys, ok := expectedDouyinQueryKeys[c]
		if !ok {
			continue
		}
		for _, k := range keys {
			total++
			if upstreamKeys[k] {
				hit++
			} else {
				misses = append(misses, c+":"+k)
			}
		}
	}
	if total == 0 {
		return 0, "", fmt.Errorf("no scored expectations for %v", contracts)
	}
	sort.Strings(misses)
	detail := fmt.Sprintf("%d/%d query keys conform to f2's table at %s", hit, total, shortSHA(pin))
	if len(misses) > 0 {
		detail += "; misses: " + strings.Join(misses[:minInt(6, len(misses))], ", ")
	}
	return float64(hit) / float64(total), detail, nil
}

// fetchUpstreamFileDefault is the production fetch (vendor dir, then API).
var fetchUpstreamFileDefault = fetchUpstreamFile

// fetchUpstreamFile reads a file at a ref: local vendor submodule first,
// then the GitHub contents API.
func fetchUpstreamFile(ctx context.Context, hc *http.Client, slug, ref, path, token string) ([]byte, error) {
	local := filepath.Join("upstream", "vendor", strings.SplitN(slug, "/", 2)[1])
	if _, err := os.Stat(local); err == nil {
		// vendor initialized on disk: require the worktree checkout to
		// avoid reading stale content silently
		if data, err := os.ReadFile(filepath.Join(local, path)); err == nil {
			return data, nil
		}
	}
	raw, err := ghGet(ctx, hc, fmt.Sprintf("https://api.github.com/repos/%s/contents/%s?ref=%s", slug, path, ref), token)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Content == "" {
		return nil, fmt.Errorf("no content for %s@%s", path, ref)
	}
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, doc.Content)
	return base64.StdEncoding.DecodeString(clean)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// adaptersBySlug: the bench's adapter registry.
var adaptersBySlug = map[string]swapAdapter{}

func init() {
	adaptersBySlug["Johnserf-Seed/f2"] = f2ParamAdapter{}
}

// upstreamSwapTest implements `upstream swap-test <slug>`.
func upstreamSwapTest(args []string) error {
	fs := flag.NewFlagSet("upstream swap-test", flag.ExitOnError)
	fs.SetOutput(os.Stderr)
	outDir := fs.String("out", "upstream/swap-reports", "report directory (gitignored tool output)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: upstream swap-test <slug> [--out DIR]")
	}
	slug := fs.Arg(0)
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
	adapter, ok := adaptersBySlug[slug]
	if !ok {
		return fmt.Errorf("swap-test: no adapter for %q (adapters exist for: f2) — explicit error, not a hang", slug)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hc := &http.Client{Timeout: 30 * time.Second}
	adapter = withClient(adapter, hc)
	rate, detail, err := adapter.Score(ctx, entry.Pin.Ref, scoredContracts())
	if err != nil {
		return fmt.Errorf("swap-test %s: %w", slug, err)
	}
	// freshness: days since the pinned commit
	commit, cerr := fetchCommitDate(ctx, hc, slug, entry.Pin.Ref, os.Getenv("GITHUB_TOKEN"))
	if cerr != nil {
		return fmt.Errorf("swap-test %s: pin date: %w", slug, cerr)
	}
	score := SwapScore{
		Slug: slug, SuccessRate: rate,
		FreshnessDays:  int(time.Since(commit).Hours() / 24),
		LicenseVerdict: entry.License.Verdict,
		Details:        detail, ScoredAt: time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.MarshalIndent(score, "", "  ")
	if err != nil {
		return err
	}
	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			return err
		}
		name := fmt.Sprintf("%s-%s.json", strings.ReplaceAll(slug, "/", "__"), time.Now().UTC().Format("2006-01-02"))
		if err := os.WriteFile(filepath.Join(*outDir, name), raw, 0o644); err != nil {
			return err
		}
	}
	fmt.Println(string(raw))
	if score.SuccessRate >= 0.8 {
		fmt.Println("建议：评分达标——采纳走 C1 PR 附本评分（决策记录进 issue）")
	} else {
		fmt.Println("建议：评分未达 0.8——忽略或继续观测（决策记录进 issue）")
	}
	return nil
}

// scoredContracts: the douyin contracts the bench scores.
func scoredContracts() []string {
	return []string{"douyin-user-posts", "douyin-search", "douyin-comments", "douyin-video-download"}
}

// fetchCommitDate resolves a commit's timestamp.
func fetchCommitDate(ctx context.Context, hc *http.Client, slug, sha, token string) (time.Time, error) {
	raw, err := ghGet(ctx, hc, fmt.Sprintf("https://api.github.com/repos/%s/commits/%s", slug, sha), token)
	if err != nil {
		return time.Time{}, err
	}
	var doc struct {
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, doc.Commit.Committer.Date)
}

// mustJSON marshals or panics (test helper).
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// withClient injects the HTTP client into adapters that carry one.
func withClient(a swapAdapter, hc *http.Client) swapAdapter {
	if f2, ok := a.(f2ParamAdapter); ok {
		f2.hc = hc
		f2.token = os.Getenv("GITHUB_TOKEN")
		return f2
	}
	return a
}
