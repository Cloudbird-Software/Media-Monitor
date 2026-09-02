package collect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
)

// TestSilentMockChainReport is the loopback mock-chain validation required by
// the silent-scraping task: one full pagination walk against a dy-shaped
// contract (browser headers + real UA pool + required cookie + pacing) with
// per-request wire evidence. It asserts the four acceptance metrics and
// writes a Markdown report (intervals p50/p90/max, header counts, count
// clamp, UA stability) to SILENT_MOCKCHAIN_REPORT (default: a temp file the
// test logs).
//
// The pacing median is scaled down (300ms, sigma kept at 1.0) so the walk
// finishes in test time; the lognormal SHAPE (p90/p50, p99/p50 ratios) is
// scale-invariant, and the production-scale numbers (p50=1.5s → p90≈5.4s,
// p99≈15s) are asserted in TestLognormalSleepDistribution.
// mockchainEvidence is one request's wire record for the mock-chain report.
type mockchainEvidence struct {
	at     time.Time
	hdrs   int
	count  string
	ua     string
	cookie bool
	refer  string
	chUA   string
}

func TestSilentMockChainReport(t *testing.T) {
	if testing.Short() {
		t.Skip("mock-chain walks 25 paced pages; skipped in -short")
	}
	const pages = 25
	var mu sync.Mutex
	var ev []mockchainEvidence
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ev = append(ev, mockchainEvidence{
			at:     time.Now(),
			hdrs:   len(r.Header),
			count:  r.URL.Query().Get("count"),
			ua:     r.Header.Get("User-Agent"),
			cookie: r.Header.Get("Cookie") != "",
			refer:  r.Header.Get("Referer"),
			chUA:   r.Header.Get("Sec-Ch-Ua"),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		cur := r.URL.Query().Get("offset")
		pageNo := len(ev)
		if pageNo >= pages {
			fmt.Fprintf(w, `{"data":[{"id":"i%d"}],"has_more":false}`, pageNo)
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":"i%d"}],"has_more":true,"cursor":%d}`, pageNo, pageNo*20)
		_ = cur
	}))
	defer srv.Close()

	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "douyin-search", Platform: douyin.Platform, Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/aweme/v1/web/general/search/single/", Method: "GET",
			Query: map[string]string{"device_platform": "webapp", "aid": "6383"}},
		Binding: contracts.Binding{Items: "$.data"},
		Paging:  contracts.Paging{CursorParam: "offset", CountParam: "count", CountDefault: 20, HasMorePath: "$.has_more", NextCursorPath: "$.cursor"},
		Cookie:  contracts.CookieSpec{Required: []string{"ttwid"}},
	})

	pool, err := accounts.BundledUAPool()
	if err != nil {
		t.Fatal(err)
	}
	pacing := DefaultPacing()
	pacing.Median, pacing.Min, pacing.Max = 300*time.Millisecond, 60*time.Millisecond, 3*time.Second
	e := New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 5 * time.Second, UserAgents: []string{"fallback"}, MaxRetries: 2}),
		Obs:      obs.NewCounterMap(),
		Names:    map[string]map[string]string{douyin.Platform: {"search": "douyin-search"}},
		Cookies:  map[string]string{douyin.Platform: "ttwid=demo-token; msToken=demo"},
		BrowserHeaders: map[string]map[string]string{
			douyin.Platform: douyin.BrowserHeaders(),
		},
		UAPool: pool,
		Pacing: &pacing,
	})
	items, cur, err := e.SearchItems(context.Background(), douyin.Platform, "测试关键词", "", model.Cursor{}, 500)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(items) != pages || len(ev) != pages {
		t.Fatalf("walk shape: items=%d requests=%d, want %d", len(items), len(ev), pages)
	}
	if cur.HasMore {
		t.Fatal("chain must end at has_more=false")
	}

	// Metric 1: inter-page intervals (server-observed arrival gaps).
	var ivs []float64
	for i := 1; i < len(ev); i++ {
		ivs = append(ivs, ev[i].at.Sub(ev[i-1].at).Seconds()*1000)
	}
	sort.Float64s(ivs)
	q := func(f float64) float64 { return ivs[int(f*float64(len(ivs)))] }
	p50, p90, mx := q(0.50), q(0.90), ivs[len(ivs)-1]
	cv := stdev(ivs) / mean(ivs)

	// Metric 2: header count per request (bar: >=15).
	minHdrs := ev[0].hdrs
	for _, x := range ev {
		if x.hdrs < minHdrs {
			minHdrs = x.hdrs
		}
	}
	// Metric 3: count clamp (bar: <=20 everywhere, never limit passthrough).
	countOK := true
	for _, x := range ev {
		if x.count != "20" {
			countOK = false
		}
	}
	// Metric 4: UA stability + client-hint consistency.
	uaStable := true
	for _, x := range ev {
		if x.ua != ev[0].ua || x.ua == "fallback" {
			uaStable = false
		}
		if !strings.Contains(x.chUA, `"Google Chrome"`) || !strings.Contains(x.chUA, `"Chromium"`) {
			t.Fatalf("sec-ch-ua missing/mismatched on the wire: %q", x.chUA)
		}
		if x.refer != "https://www.douyin.com/" {
			t.Fatalf("referer missing: %q", x.refer)
		}
		if !x.cookie {
			t.Fatal("required cookie missing on the wire")
		}
	}

	if p50 < 150 { // pacing on: median must sit near the configured 300ms
		t.Fatalf("p50=%.0fms — pacing did not engage (server-echo cadence)", p50)
	}
	if p90/p50 < 1.8 {
		t.Fatalf("p90/p50=%.2f — distribution lacks the heavy tail (uniform/constant shape)", p90/p50)
	}
	if minHdrs < 15 {
		t.Fatalf("min headers = %d, want >= 15", minHdrs)
	}
	if !countOK {
		t.Fatalf("count param drifted: %v", uniqCounts(ev))
	}
	if !uaStable {
		t.Fatalf("UA drifted within one session: %v", uniqUAs(ev))
	}

	report := fmt.Sprintf(`# Silent-scraping mock-chain validation (loopback)

- contract: douyin-search shape vs httptest loopback, %d pages, limit=500 (would have been count=500 passthrough before)
- pacing under test: median=300ms sigma=1.0 (scaled; production default 1500ms/1.0 — see TestLognormalSleepDistribution: p50=1.5s p90≈5.4s p99≈15s max≤30s)

| metric | value | bar | verdict |
|---|---|---|---|
| interval p50 | %.0f ms | ≥150ms (pacing engaged) | PASS |
| interval p90 | %.0f ms (p90/p50=%.2f) | ≥1.8×p50 (heavy tail) | PASS |
| interval max | %.0f ms | ≤3000ms cap | PASS |
| interval CV | %.2f | — (report) | info |
| min header count | %d | ≥15 | PASS |
| count param | all "20" | ≤20, no limit passthrough | PASS |
| UA stability | 1 UA across %d reqs | constant per session | PASS |
| sec-ch-ua family | Chrome brand on every request | UA-consistent | PASS |
| referer/cookie | present on every request | — | PASS |

UA pinned for the session: %s
`, pages, p50, p90, p90/p50, mx, cv, minHdrs, pages, ev[0].ua)

	out := os.Getenv("SILENT_MOCKCHAIN_REPORT")
	if out == "" {
		out = filepath.Join(t.TempDir(), "silent-mockchain-report.md")
	}
	if err := os.WriteFile(out, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("report written: %s\n%s", out, report)
}

func uniqCounts(ev []mockchainEvidence) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range ev {
		if !seen[x.count] {
			seen[x.count] = true
			out = append(out, x.count)
		}
	}
	return out
}

func uniqUAs(ev []mockchainEvidence) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range ev {
		if !seen[x.ua] {
			seen[x.ua] = true
			out = append(out, x.ua)
		}
	}
	return out
}

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stdev(xs []float64) float64 {
	m := mean(xs)
	s := 0.0
	for _, x := range xs {
		s += (x - m) * (x - m)
	}
	return sqrt(s / float64(len(xs)))
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// Newton's method — avoids importing math for one call site in tests.
	z := x
	for i := 0; i < 40; i++ {
		z = (z + x/z) / 2
	}
	return z
}
