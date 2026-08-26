package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

// predItem builds one raw record (digg count, create_time unix seconds);
// stats=false omits the statistics object entirely.
func predItem(id string, digg int64, ts int64, stats bool) map[string]any {
	rec := map[string]any{"aweme_id": id, "desc": "d" + id, "create_time": ts, "type": 1,
		"author": map[string]any{"sec_uid": "s1", "nickname": "n"}}
	if stats {
		rec["statistics"] = map[string]any{"digg_count": digg, "comment_count": 10, "collect_count": 10, "share_count": 10}
	}
	return rec
}

// predServer serves one page of records per request; has_more stays true
// until the last page so only the predicate (or limit) stops the walk early.
type predServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	pulled int
}

func newPredServer(t *testing.T, pages ...[]map[string]any) *predServer {
	t.Helper()
	ps := &predServer{}
	ps.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ps.mu.Lock()
		i := ps.pulled
		ps.pulled++
		ps.mu.Unlock()
		if i >= len(pages) {
			t.Errorf("page %d beyond %d prepared", i, len(pages))
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"aweme_list": pages[i], "has_more": i < len(pages)-1, "max_cursor": (i + 1) * 10})
	}))
	return ps
}

func predEngine(t *testing.T, srv *httptest.Server) *Engine {
	t.Helper()
	dir := t.TempDir()
	contract := fmt.Sprintf(`{"name":"pred-user-posts","platform":"douyin","category":"user_posts","version":"1","transport":{"base_url":%q,"path":"/post/","method":"GET","placeholders":["sec_user_id"]},"signature":{"required":["a_bogus"]},"binding":{"items":"$.aweme_list"},"paging":{"cursor_param":"max_cursor","count_param":"count","count_default":20,"has_more_path":"$.has_more","next_cursor_path":"$.max_cursor"}}`, srv.URL)
	if err := os.WriteFile(filepath.Join(dir, "pred-user-posts.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, dir); err != nil {
		t.Fatal(err)
	}
	return New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}),
		Obs:      obs.NewCounterMap(),
		Signers: map[string]httpclient.Signer{"douyin": httpclient.StaticSigner{Fn: func(context.Context, string, string, map[string]string) (map[string]string, error) {
			return map[string]string{"a_bogus": "x"}, nil
		}}},
	})
}

// fetchPred runs fetchPagesWith over the fake contract.
func fetchPred(t *testing.T, e *Engine, opt BacktrackOptions, now time.Time, limit int) ([]map[string]any, model.Cursor, error) {
	t.Helper()
	pred, err := compileStopPredicate(opt, now)
	if err != nil {
		return nil, model.Cursor{}, err
	}
	return e.fetchPagesWith(context.Background(), "pred-user-posts",
		map[string]string{"sec_user_id": "s1"}, nil, model.Cursor{}, limit, pred)
}

// TestPredicateVShapeConsecutiveStop: a v-shaped engagement series — the
// single low item between highs must NOT truncate; N consecutive low items
// do stop the walk (IR AC-4 / BEH-1 / D-6).
func TestPredicateVShapeConsecutiveStop(t *testing.T) {
	now := time.Unix(1780500000, 0)
	mk := func(id string, digg int64) []map[string]any {
		return []map[string]any{predItem(id, digg, now.Unix()-int64(id[0]-'0')*100, true)}
	}
	ps := newPredServer(t,
		mk("1", 50000), // high
		mk("2", 10),    // low (single — must not truncate)
		mk("3", 60000), // high (resets)
		mk("4", 10),    // low 1/3
		mk("5", 10),    // low 2/3
		mk("6", 10),    // low 3/3 -> stop
		mk("7", 90000), // must never be fetched
	)
	defer ps.srv.Close()
	e := predEngine(t, ps.srv)
	recs, _, err := fetchPred(t, e, BacktrackOptions{
		MinEngagement:        &EngagementFloor{Metric: "digg", Threshold: 1000},
		StopAfterConsecutive: 3,
	}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range recs {
		ids = append(ids, r["aweme_id"].(string))
	}
	if strings.Join(ids, ",") != "1,2,3,4,5,6" {
		t.Fatalf("emitted %v, want 1..6 (low items stay emitted, listing semantics)", ids)
	}
	ps.mu.Lock()
	if ps.pulled != 6 {
		t.Fatalf("pages pulled = %d, want 6 (page 7 never fetched)", ps.pulled)
	}
	ps.mu.Unlock()
}

// TestPredicateDefaultStopAfterFive: without StopAfterConsecutive the
// default N=5 applies (IR BEH-1).
func TestPredicateDefaultStopAfterFive(t *testing.T) {
	now := time.Unix(1780500000, 0)
	var pages [][]map[string]any
	for i := 0; i < 9; i++ {
		pages = append(pages, []map[string]any{predItem(fmt.Sprint(i+1), 5, now.Unix()-int64(i)*100-100, true)})
	}
	ps := newPredServer(t, pages...)
	defer ps.srv.Close()
	recs, _, err := fetchPred(t, predEngine(t, ps.srv), BacktrackOptions{MinEngagement: &EngagementFloor{Metric: "digg", Threshold: 1000}}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 {
		t.Fatalf("records = %d, want 5 (default N=5)", len(recs))
	}
}

// TestPredicateMissingStatsNeutral: items without the metric neither reset
// nor increment the consecutive-low counter (IR BEH-3).
func TestPredicateMissingStatsNeutral(t *testing.T) {
	now := time.Unix(1780500000, 0)
	low := func(id string) []map[string]any { return []map[string]any{predItem(id, 5, now.Unix()-100, true)} }
	missing := func(id string) []map[string]any { return []map[string]any{predItem(id, 0, now.Unix()-200, false)} }
	ps := newPredServer(t,
		low("1"),     // low 1/3
		missing("2"), // neutral — counter stays 1
		low("3"),     // low 2/3 (would be 1/2 if neutral reset the count)
		missing("4"), // neutral
		low("5"),     // low 3/3 -> stop
		low("never"), // not fetched
	)
	defer ps.srv.Close()
	e := predEngine(t, ps.srv)
	recs, _, err := fetchPred(t, e, BacktrackOptions{
		MinEngagement:        &EngagementFloor{Metric: "digg", Threshold: 1000},
		StopAfterConsecutive: 3,
	}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 {
		t.Fatalf("records = %d, want 5 (neutrals neither reset nor increment)", len(recs))
	}
	ps.mu.Lock()
	if ps.pulled != 5 {
		t.Fatalf("pages pulled = %d, want 5", ps.pulled)
	}
	ps.mu.Unlock()
}

// TestPredicateWindowCutoff: items older than the window stop the walk and
// are not emitted; the stop item and everything after stay out (AC-1).
func TestPredicateWindowCutoff(t *testing.T) {
	now := time.Unix(1780500000, 0)
	ps := newPredServer(t,
		[]map[string]any{predItem("in1", 1, now.Unix()-100, true), predItem("in2", 2, now.Unix()-200, true)},
		[]map[string]any{predItem("old1", 3, now.AddDate(0, -7, 0).Unix(), true), predItem("old2", 4, 0, true)},
	)
	defer ps.srv.Close()
	e := predEngine(t, ps.srv)
	recs, cur, err := fetchPred(t, e, BacktrackOptions{WindowMonths: 6}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2 in-window items only (got %v)", len(recs), recs)
	}
	if recs[0]["aweme_id"] != "in1" || recs[1]["aweme_id"] != "in2" {
		t.Fatalf("emitted wrong items: %v", recs)
	}
	if cur.Page == 0 {
		t.Fatal("returned cursor must carry the position for resumption")
	}
}

// TestPredicateResumableCursor: after a predicate stop the returned cursor
// resumes from the stopping page's next position (AC-5) — continuing the
// walk from it does not re-emit already returned items.
func TestPredicateResumableCursor(t *testing.T) {
	now := time.Unix(1780500000, 0)
	ps := newPredServer(t,
		[]map[string]any{predItem("1", 5, now.Unix()-100, true), predItem("2", 5, now.Unix()-200, true), predItem("3", 5, now.Unix()-300, true)},
		[]map[string]any{predItem("4", 99000, now.Unix()-400, true), predItem("5", 5, now.Unix()-500, true)},
	)
	defer ps.srv.Close()
	e := predEngine(t, ps.srv)
	recs1, cur, err := fetchPred(t, e, BacktrackOptions{MinEngagement: &EngagementFloor{Metric: "digg", Threshold: 1000}, StopAfterConsecutive: 3}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs1) != 3 {
		t.Fatalf("first pass = %d items, want 3 consecutive lows then stop", len(recs1))
	}
	// Resume: page 2 starts with a high item, then one low — with N=3 the
	// walk continues to the end (has_more false).
	pred, err := compileStopPredicate(BacktrackOptions{MinEngagement: &EngagementFloor{Metric: "digg", Threshold: 1000}, StopAfterConsecutive: 3}, now)
	if err != nil {
		t.Fatal(err)
	}
	recs2, _, err := e.fetchPagesWith(context.Background(), "pred-user-posts",
		map[string]string{"sec_user_id": "s1"}, nil, cur, 0, pred)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range recs2 {
		ids = append(ids, r["aweme_id"].(string))
	}
	if strings.Join(ids, ",") != "4,5" {
		t.Fatalf("resumed pass = %v, want 4,5 (no re-emission)", ids)
	}
}

// TestPredicateCompileValidation: zero options compile to a nil predicate
// (legacy behavior); an unknown metric fails closed at compile.
func TestPredicateCompileValidation(t *testing.T) {
	p, err := compileStopPredicate(BacktrackOptions{}, time.Now())
	if err != nil || p != nil {
		t.Fatalf("zero options = (%v, %v), want (nil, nil)", p, err)
	}
	if _, err := compileStopPredicate(BacktrackOptions{MinEngagement: &EngagementFloor{Metric: "likes", Threshold: 1}}, time.Now()); err == nil {
		t.Fatal("unknown metric must be rejected")
	}
}

// TestPredicateNotInContractSchema: the backtrack predicate is collect-time
// behavior, never contract schema (AC-6 / IFACE-1): no contract under
// adapt/contracts may carry predicate keys.
func TestPredicateNotInContractSchema(t *testing.T) {
	dir := testkit.ContractsDir(t, 2)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"window_months", "min_engagement", "stop_after_consecutive"}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range banned {
			if strings.Contains(string(raw), b) {
				t.Fatalf("contract %s carries predicate key %q (predicates are collect-time, IFACE-1)", e.Name(), b)
			}
		}
	}
}
