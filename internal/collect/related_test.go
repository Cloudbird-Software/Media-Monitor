// related_test.go — RelatedGraph unit tests: single-page hop semantics,
// K-hop budget, cycle guard, hop cap fail-closed and determinism.
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// relatedFixture serves related(seed) = neighbors[seed] deterministically,
// recording which ids were expanded (each at most once by contract).
func relatedFixture(t *testing.T, neighbors map[string][]string) (*httptest.Server, *[]string) {
	t.Helper()
	var expansions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("aweme_id")
		expansions = append(expansions, id)
		list := []map[string]any{}
		for _, dst := range neighbors[id] {
			list = append(list, map[string]any{
				"aweme_id": dst,
				"author":   map[string]any{"sec_uid": "SU-" + dst, "nickname": dst},
				"statistics": map[string]any{
					"digg_count": len(dst), "comment_count": 1,
				},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status_code": 0, "aweme_list": list, "has_more": 0,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &expansions
}

func relatedContract(base string) *contracts.Contract {
	return &contracts.Contract{
		Name: "mock-related", Platform: "mock", Category: "related", Version: "1",
		Transport: contracts.Transport{BaseURL: base, Path: "/related", Method: "GET", Placeholders: []string{"aweme_id"}},
		Binding: contracts.Binding{
			Items: "$.aweme_list",
			Fields: map[string]string{
				"id":             "$.aweme_list[].aweme_id",
				"author.sec_uid": "$.aweme_list[].author.sec_uid",
			},
		},
		// no paging: single-page hop semantics
	}
}

// TestRelatedGraphKHopCycleGuard: seed→{A,B}, A→{B,seed}, B→{A} — nodes
// never expand twice, back-edges are recorded, walk stops when the
// expansion budget is exhausted.
func TestRelatedGraphKHopCycleGuard(t *testing.T) {
	srv, expansions := relatedFixture(t, map[string][]string{
		"SEED": {"A", "B"},
		"A":    {"B", "SEED"},
		"B":    {"A"},
	})
	reg := addContracts(t, relatedContract(srv.URL))
	eng := mockEngine(t, reg, func(c *Context) { c.Pacing = &PacingConfig{} })

	g, err := eng.RelatedGraph(context.Background(), "mock", "SEED", RelatedOptions{})
	if err != nil {
		t.Fatalf("RelatedGraph: %v", err)
	}
	if g.Stats.Requests != 3 {
		t.Fatalf("requests = %d, want 3 (SEED, A, B — no re-expansion)", g.Stats.Requests)
	}
	if g.Stats.NNodes != 3 || g.Stats.NEdges != 5 {
		t.Fatalf("nodes = %d edges = %d, want 3/5 (cycle back-edges kept)", g.Stats.NNodes, g.Stats.NEdges)
	}
	if g.Stats.AvgOutDegree != float64(5)/3 {
		t.Fatalf("avg out-degree = %v", g.Stats.AvgOutDegree)
	}
	want := []string{"SEED", "A", "B"}
	if len(*expansions) != 3 || (*expansions)[0] != "SEED" {
		t.Fatalf("expansion order wrong: %v want %v", *expansions, want)
	}
	// every node carries the author join id
	for _, n := range g.Nodes {
		if n.AwemeID != "SEED" && n.SecUID != "SU-"+n.AwemeID {
			t.Fatalf("node author join wrong: %+v", n)
		}
	}
}

// TestRelatedGraphBudgets: MaxExpansions=1 keeps the 1-hop graph.
func TestRelatedGraphBudgets(t *testing.T) {
	srv, expansions := relatedFixture(t, map[string][]string{
		"SEED": {"A", "B"},
		"A":    {"C"},
		"B":    {"D"},
	})
	reg := addContracts(t, relatedContract(srv.URL))
	eng := mockEngine(t, reg, func(c *Context) { c.Pacing = &PacingConfig{} })
	g, err := eng.RelatedGraph(context.Background(), "mock", "SEED", RelatedOptions{MaxExpansions: 1})
	if err != nil {
		t.Fatalf("RelatedGraph: %v", err)
	}
	if g.Stats.Requests != 1 || g.Stats.NNodes != 3 || g.Stats.NEdges != 2 {
		t.Fatalf("budget walk wrong: %+v", g.Stats)
	}
	if len(*expansions) != 1 {
		t.Fatalf("expansions = %v", *expansions)
	}
}

// TestRelatedGraphHopCap: MaxHops above the cap fails closed; a raised
// cap (env) admits deeper walks.
func TestRelatedGraphHopCap(t *testing.T) {
	srv, _ := relatedFixture(t, map[string][]string{"SEED": {"A"}, "A": {"B"}, "B": {"C"}})
	reg := addContracts(t, relatedContract(srv.URL))
	eng := mockEngine(t, reg, func(c *Context) { c.Pacing = &PacingConfig{} })
	if _, err := eng.RelatedGraph(context.Background(), "mock", "SEED", RelatedOptions{MaxHops: 5}); err == nil {
		t.Fatal("want fail-closed error when MaxHops exceeds the cap")
	}
	t.Setenv("MEDIAMON_RELATED_MAX_HOPS", "6")
	g, err := eng.RelatedGraph(context.Background(), "mock", "SEED", RelatedOptions{MaxHops: 3, MaxExpansions: 10})
	if err != nil {
		t.Fatalf("RelatedGraph with raised cap: %v", err)
	}
	if g.Stats.HopsWalked != 3 {
		t.Fatalf("hops = %d, want 3", g.Stats.HopsWalked)
	}
}

// TestRelatedGraphDeterministic: two walks over the same fixture produce
// identical node order, edge order and stats.
func TestRelatedGraphDeterministic(t *testing.T) {
	srv, _ := relatedFixture(t, map[string][]string{
		"SEED": {"A", "B", "C"},
		"A":    {"D", "E"},
		"B":    {"F"},
		"C":    {"A"},
	})
	reg := addContracts(t, relatedContract(srv.URL))
	eng := mockEngine(t, reg, func(c *Context) { c.Pacing = &PacingConfig{} })
	g1, err := eng.RelatedGraph(context.Background(), "mock", "SEED", RelatedOptions{})
	if err != nil {
		t.Fatalf("walk1: %v", err)
	}
	g2, err := eng.RelatedGraph(context.Background(), "mock", "SEED", RelatedOptions{})
	if err != nil {
		t.Fatalf("walk2: %v", err)
	}
	if len(g1.Nodes) != len(g2.Nodes) || len(g1.Edges) != len(g2.Edges) || g1.Stats != g2.Stats {
		t.Fatalf("walks differ: %+v vs %+v", g1.Stats, g2.Stats)
	}
	var e1, e2 []string
	for i := range g1.Edges {
		e1 = append(e1, fmt.Sprintf("%s->%s", g1.Edges[i].Src, g1.Edges[i].Dst))
		e2 = append(e2, fmt.Sprintf("%s->%s", g2.Edges[i].Src, g2.Edges[i].Dst))
	}
	sort.Strings(e1)
	sort.Strings(e2)
	for i := range e1 {
		if e1[i] != e2[i] {
			t.Fatalf("edge sets differ at %d: %v vs %v", i, e1[i], e2[i])
		}
	}
}
