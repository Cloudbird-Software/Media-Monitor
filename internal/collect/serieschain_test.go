// serieschain_test.go — SeriesChain unit tests: mix episode-map face
// (document-level JSON string map), series walk-order face, cursor closure
// and has_more termination, dedup/rewind guard, count clamp, fail-closed
// rows (bad kind / empty id / undeclared contract).
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

func mixContract(srv *httptest.Server) *contracts.Contract {
	return &contracts.Contract{
		Name: "mock-mix", Platform: "mock", Category: "mix", Version: "1",
		Transport: contracts.Transport{
			BaseURL: srv.URL, Path: "/mix/aweme", Method: http.MethodGet,
			Placeholders: []string{"mix_id"},
		},
		Binding: contracts.Binding{
			Items:  "$.aweme_list",
			Fields: map[string]string{"episode_map": "$.item_id_to_episode"},
		},
		Paging: contracts.Paging{
			CursorParam: "cursor", CountParam: "count", CountDefault: 6,
			HasMorePath: "$.has_more", NextCursorPath: "$.cursor",
		},
	}
}

func seriesContract(srv *httptest.Server) *contracts.Contract {
	return &contracts.Contract{
		Name: "mock-series", Platform: "mock", Category: "series", Version: "1",
		Transport: contracts.Transport{
			BaseURL: srv.URL, Path: "/series/aweme", Method: http.MethodGet,
			Placeholders: []string{"series_id"},
		},
		Binding: contracts.Binding{Items: "$.aweme_list"},
		Paging: contracts.Paging{
			CursorParam: "cursor", CountParam: "count", CountDefault: 6,
			HasMorePath: "$.has_more", NextCursorPath: "$.max_cursor",
		},
	}
}

// mixHandler serves a deterministic mix chain of n episodes through the
// count-sized cursor window, with the item_id_to_episode JSON-string map.
func mixHandler(n int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cursor := 0
		fmt.Sscan(r.URL.Query().Get("cursor"), &cursor)
		count := 6
		fmt.Sscan(r.URL.Query().Get("count"), &count)
		end := cursor + count
		if end > n {
			end = n
		}
		items := []map[string]any{}
		epMap := map[string]int{}
		for i := cursor; i < end; i++ {
			id := fmt.Sprintf("7000000000000000%03d", i)
			items = append(items, map[string]any{
				"aweme_id": id, "desc": fmt.Sprintf("第%d集", i+1),
				"statistics": map[string]any{"digg_count": 1000 + i},
			})
			epMap[id] = i + 1
		}
		em, _ := json.Marshal(epMap)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"aweme_list": items, "cursor": end, "has_more": end < n,
			"item_id_to_episode": string(em), "status_code": 0,
		})
	}
}

func mixServer(t *testing.T, n int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(mixHandler(n))
	t.Cleanup(srv.Close)
	return srv
}

// seriesServer serves a deterministic series chain of n episodes WITHOUT the
// episode map (walk-order numbering), plus a rewind at the end (the cursor
// jumps back to 0 with has_more still true — the rewind guard must stop).
func seriesServer(t *testing.T, n int, rewind bool) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := 0
		fmt.Sscan(r.URL.Query().Get("cursor"), &cursor)
		count := 6
		fmt.Sscan(r.URL.Query().Get("count"), &count)
		end := cursor + count
		if end > n {
			end = n
		}
		items := []map[string]any{}
		for i := cursor; i < end; i++ {
			items = append(items, map[string]any{
				"aweme_id": fmt.Sprintf("7100000000000000%03d", i),
				"desc":     fmt.Sprintf("episode %d", i+1),
			})
		}
		mu.Lock()
		next := end
		hasMore := end < n
		if rewind && end >= n { // window edge rewinds like the ks "1" loop
			next = 0
			hasMore = true
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"aweme_list": items, "max_cursor": next, "has_more": hasMore,
			"status_code": 0,
		})
	}))
}

func chainEngine(t *testing.T, cs ...*contracts.Contract) *Engine {
	t.Helper()
	return mockEngine(t, addContracts(t, cs...), func(ctx *Context) {
		ctx.Pacing = &PacingConfig{}
		ctx.Names = map[string]map[string]string{cs[0].Platform: {
			"mix": cs[0].Name, "series": cs[len(cs)-1].Name,
		}}
	})
}

func TestSeriesChainMixEpisodeMapClosure(t *testing.T) {
	srv := mixServer(t, 13)
	eng := chainEngine(t, mixContract(srv))
	out, err := eng.SeriesChain(context.Background(), "mock", "mix", "7001234567890123456", SeriesOptions{})
	if err != nil {
		t.Fatalf("SeriesChain(mix): %v", err)
	}
	if out.TotalEpisodes != 13 || out.Pages != 3 || out.UniqueIDs != 13 {
		t.Fatalf("mix closure wrong: %+v", out)
	}
	if out.EpisodeSource != "item_id_to_episode" {
		t.Fatalf("episode source = %q", out.EpisodeSource)
	}
	eps := sortedEpisodeNumbers(out.Episodes)
	for i, ep := range eps {
		if ep != i+1 {
			t.Fatalf("episode numbers = %v, want contiguous 1..13", eps)
		}
	}
	if len(out.MissingEps) != 0 || out.Dupes != 0 || out.HasMore {
		t.Fatalf("chain not closed: missing=%v dupes=%d has_more=%v", out.MissingEps, out.Dupes, out.HasMore)
	}
	// Determinism: repeat walk → same episode ids in the same order.
	out2, err := eng.SeriesChain(context.Background(), "mock", "mix", "7001234567890123456", SeriesOptions{})
	if err != nil {
		t.Fatalf("SeriesChain(mix) repeat: %v", err)
	}
	for i := range out.Episodes {
		if out.Episodes[i].Item.ID != out2.Episodes[i].Item.ID ||
			out.Episodes[i].Episode != out2.Episodes[i].Episode {
			t.Fatalf("repeat walk differs at %d", i)
		}
	}
}

func TestSeriesChainSeriesWalkOrderAndRewindGuard(t *testing.T) {
	srv := seriesServer(t, 33, true)
	eng := chainEngine(t, seriesContract(srv))
	out, err := eng.SeriesChain(context.Background(), "mock", "series", "7001234567890123456", SeriesOptions{})
	if err != nil {
		t.Fatalf("SeriesChain(series): %v", err)
	}
	if out.TotalEpisodes != 33 || out.UniqueIDs != 33 {
		t.Fatalf("series closure wrong: %+v", out)
	}
	if out.EpisodeSource != "walk_order" {
		t.Fatalf("episode source = %q", out.EpisodeSource)
	}
	eps := sortedEpisodeNumbers(out.Episodes)
	for i, ep := range eps {
		if ep != i+1 {
			t.Fatalf("walk-order episodes = %v, want contiguous 1..33", eps)
		}
	}
	// The window-edge rewind (cursor jumps back to 0 with has_more=true)
	// re-serves page 1 (6 dupes on the 7th fetch) — the zero-new-page guard
	// stops there without duplicating any episode.
	if out.Pages != 7 || out.Dupes != 6 {
		t.Fatalf("rewind guard wrong: pages=%d dupes=%d, want 7/6", out.Pages, out.Dupes)
	}
}

func TestSeriesChainLimitAndCountClamp(t *testing.T) {
	var mu sync.Mutex
	var counts []string
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts = append(counts, r.URL.Query().Get("count"))
		mu.Unlock()
		mixHandler(40)(w, r) // reuse the window logic
	}))
	defer wrapped.Close()
	eng := chainEngine(t, mixContract(wrapped))
	out, err := eng.SeriesChain(context.Background(), "mock", "mix", "m1", SeriesOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SeriesChain(limit): %v", err)
	}
	if out.TotalEpisodes != 10 || out.Pages != 2 {
		t.Fatalf("limit wrong: %+v", out)
	}
	mu.Lock()
	if len(counts) == 0 || counts[0] != "6" {
		mu.Unlock()
		t.Fatalf("count param = %v, want contract count_default 6", counts)
	}
	mu.Unlock()
	// Count above the per-request cap clamps down.
	out2, err := eng.SeriesChain(context.Background(), "mock", "mix", "m1", SeriesOptions{Count: 99, MaxPages: 1})
	if err != nil {
		t.Fatalf("SeriesChain(count clamp): %v", err)
	}
	if out2.Pages != 1 || len(out2.Episodes) > 20 {
		t.Fatalf("count clamp wrong: %+v", out2)
	}
}

func TestSeriesChainFailClosedRows(t *testing.T) {
	srv := mixServer(t, 5)
	eng := chainEngine(t, mixContract(srv))
	if _, err := eng.SeriesChain(context.Background(), "mock", "collect", "x", SeriesOptions{}); err == nil || !strings.Contains(err.Error(), "not in {mix,series}") {
		t.Fatalf("bad kind must fail closed, got %v", err)
	}
	if _, err := eng.SeriesChain(context.Background(), "mock", "mix", "  ", SeriesOptions{}); err == nil || !strings.Contains(err.Error(), "empty mix id") {
		t.Fatalf("empty id must fail closed, got %v", err)
	}
	if _, err := eng.SeriesChain(context.Background(), "nomock", "mix", "x", SeriesOptions{}); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared platform contract must fail closed, got %v", err)
	}
	// Unknown id on a tolerant face: clean empty success (zero-episode chain,
	// one page fetch, has_more ended — three-valued outcome discipline).
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"aweme_list": []any{}, "cursor": 0, "has_more": false,
			"item_id_to_episode": "{}", "status_code": 0,
		})
	}))
	defer empty.Close()
	eng2 := chainEngine(t, mixContract(empty))
	out2, err := eng2.SeriesChain(context.Background(), "mock", "mix", "unknown", SeriesOptions{})
	if err != nil || out2.TotalEpisodes != 0 || out2.Pages != 1 || out2.HasMore {
		t.Fatalf("clean empty chain wrong: %+v err=%v", out2, err)
	}
}
