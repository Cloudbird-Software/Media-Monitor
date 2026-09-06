package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

// TestPageCountForClamp verifies the count math (report item 3 / C1):
// limit is a walk bound, never a page size; page size is always clamped to
// the cap (default 20); contract count_default applies when no limit is set.
func TestPageCountForClamp(t *testing.T) {
	c := &contracts.Contract{Paging: contracts.Paging{CountParam: "count", CountDefault: 20}}
	cases := []struct {
		limit, want int
	}{
		{0, 20},    // no limit → contract default
		{10, 10},   // small limit → smaller page (more human)
		{20, 20},   // exactly the cap
		{60, 20},   // t02: --limit 60 must NOT produce count=60
		{100, 20},  // same for 100
		{1000, 20}, // unbounded-ish stays at the cap
	}
	for _, tc := range cases {
		if got := pageCountFor(c, tc.limit); got != tc.want {
			t.Fatalf("pageCountFor(limit=%d) = %d, want %d", tc.limit, got, tc.want)
		}
	}
	// Contract without count_default: cap is the floor.
	c2 := &contracts.Contract{Paging: contracts.Paging{CountParam: "count"}}
	if got := pageCountFor(c2, 0); got != 20 {
		t.Fatalf("no count_default: got %d, want cap 20", got)
	}
	// Configurable cap via env.
	t.Setenv("MEDIAMON_MAX_COUNT", "50")
	if got := pageCountFor(c, 60); got != 50 {
		t.Fatalf("MEDIAMON_MAX_COUNT=50: got %d, want 50", got)
	}
	if got := pageCountFor(c, 30); got != 30 {
		t.Fatalf("MEDIAMON_MAX_COUNT=50, limit 30: got %d, want 30", got)
	}
}

// TestCountNeverPassthrough walks a real mock chain with --limit 60 and
// verifies every request's count param is ≤20 while the walk still collects
// 60 records across pages (multi-page, human-shaped).
func TestCountNeverPassthrough(t *testing.T) {
	var counts []string
	total := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts = append(counts, r.URL.Query().Get("count"))
		total++
		// 20 items per page, two pages then a short tail page (55 total).
		n := 20
		if total == 3 {
			n = 15
		}
		body := `{"data":[`
		for i := 0; i < n; i++ {
			if i > 0 {
				body += ","
			}
			body += `{"id":"i"}`
		}
		more := total < 3
		body += `],"has_more":` + boolJSON(more) + `,"cursor":"c` + itoa(total) + `"}`
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer srv.Close()
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "clamp-search", Platform: "mock", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/list", Method: "GET"},
		Binding:   contracts.Binding{Items: "$.data"},
		Paging:    contracts.Paging{CursorParam: "cursor", CountParam: "count", CountDefault: 20, HasMorePath: "$.has_more", NextCursorPath: "$.cursor"},
	})
	e := New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}),
		Obs:      obs.NewCounterMap(),
		Names:    map[string]map[string]string{"mock": {"search": "clamp-search"}},
	})
	items, _, err := e.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 55 {
		t.Fatalf("records = %d, want 55 (limit only bounds the walk)", len(items))
	}
	if len(counts) != 3 {
		t.Fatalf("pages = %d, want 3", len(counts))
	}
	for i, c := range counts {
		if c != "20" {
			t.Fatalf("page %d count=%q, want \"20\" (clamped, never 60)", i+1, c)
		}
	}
}

// TestLimitZeroSafeDefault: --limit 0 no longer means "page until the guard
// trips with an error and the data is thrown away" — the walk runs at the
// safe page size and stops at the natural end (has_more=false).
func TestLimitZeroSafeDefault(t *testing.T) {
	var counts []string
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		counts = append(counts, r.URL.Query().Get("count"))
		if served >= 3 {
			w.Write([]byte(`{"data":[{"id":"x"}],"has_more":false}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"x"}],"has_more":true,"cursor":"n"}`))
	}))
	defer srv.Close()
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "zero-search", Platform: "mock", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/list", Method: "GET"},
		Binding:   contracts.Binding{Items: "$.data"},
		Paging:    contracts.Paging{CursorParam: "cursor", CountParam: "count", CountDefault: 20, HasMorePath: "$.has_more", NextCursorPath: "$.cursor"},
	})
	e := New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}),
		Obs:      obs.NewCounterMap(),
		Names:    map[string]map[string]string{"mock": {"search": "zero-search"}},
	})
	items, cur, err := e.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || served != 3 {
		t.Fatalf("records=%d served=%d, want 3/3 (natural stop)", len(items), served)
	}
	if cur.HasMore {
		t.Fatal("walk must report has_more=false at the natural end")
	}
	for i, c := range counts {
		if c != "20" {
			t.Fatalf("page %d count=%q, want 20 (safe default, not unlimited)", i+1, c)
		}
	}
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func itoa(n int) string {
	return string(rune('0' + n))
}
