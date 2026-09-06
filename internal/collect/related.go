// related.go — RelatedGraph, the related-recommendation graph atom
// (capability proposal E / doc F, P1): given one content id, produce the
// 1-hop adjacency and a budgeted K-hop traversal — content nodes, related
// edges, per-node author ids (joinable into capability A's dossier) and
// graph statistics.
//
// Design notes (atom boundary / schema trade-offs):
//   - The related face is SINGLE-PAGE (10 items, has_more=1 but no cursor
//     on both the synth oracle and the real site): a "hop" is one request,
//     and graph breadth comes from K-hop expansion, not pagination.
//   - K-hop discipline: breadth-first over the response order (deterministic),
//     a node expands at most once (cycle guard — edges into already-known
//     nodes are recorded but never re-expanded), and three budgets bound the
//     walk: MaxHops (default 2, hard cap 3 — MEDIAMON_RELATED_MAX_HOPS
//     raises the cap), MaxExpansions (default 8 = the probe baseline's
//     8-request walk) and MaxNodes. All requests ride the same engine Fetch
//     (headers/UA/cookie/pacing discipline inherited).
//   - Baseline shape (probe, full oracle dataset): 8 expansions = 80 edges,
//     avg out-degree 10.0; from one seed with K=2/expansions=8 the window
//     overlap yields 18 nodes (documented expectation for the e2e target).
package collect

import (
	"context"
	"fmt"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// RelatedNode is one content node of the graph.
type RelatedNode struct {
	AwemeID   string `json:"aweme_id"`
	SecUID    string `json:"sec_uid,omitempty"`
	DiggCount int64  `json:"digg_count"`
}

// RelatedEdge is one related-recommendation edge (src → dst).
type RelatedEdge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

// RelatedStats summarizes the walked graph.
type RelatedStats struct {
	NNodes       int     `json:"n_nodes"`
	NEdges       int     `json:"n_edges"`
	AvgOutDegree float64 `json:"avg_out_degree"` // edges / expansions
	HopsWalked   int     `json:"hops_walked"`
	Requests     int     `json:"requests"`     // == expansions issued
	NodesCapped  int     `json:"nodes_capped"` // frontier dropped by budgets
}

// RelatedGraph is the collected adjacency of one seed content id.
type RelatedGraph struct {
	SeedID string        `json:"seed_id"`
	Nodes  []RelatedNode `json:"nodes"`
	Edges  []RelatedEdge `json:"edges"`
	Stats  RelatedStats  `json:"stats"`
}

// RelatedOptions bounds the K-hop traversal.
type RelatedOptions struct {
	// MaxHops is the K in K-hop (0 = default 2). Values above the hop cap
	// (default 3, MEDIAMON_RELATED_MAX_HOPS) fail closed.
	MaxHops int
	// MaxExpansions caps the total related requests (0 = default 8,
	// MEDIAMON_RELATED_MAX_EXPANSIONS overrides).
	MaxExpansions int
	// MaxNodes caps the node budget (0 = derived from the expansion budget).
	MaxNodes int
}

// related defaults (env-overridable; see resolve* functions).
const (
	defaultRelatedMaxHops       = 2
	defaultRelatedHopCap        = 3
	defaultRelatedMaxExpansions = 8
)

// resolveRelatedMaxHops validates the requested hop count against the cap.
func resolveRelatedMaxHops(requested int) (int, error) {
	hops := requested
	if hops == 0 {
		hops = defaultRelatedMaxHops
	}
	if hops < 0 {
		return 0, fmt.Errorf("collect: related max_hops must be >= 0")
	}
	cap := defaultRelatedHopCap
	if n, ok := envInt("MEDIAMON_RELATED_MAX_HOPS"); ok && n > 0 {
		cap = n
	}
	if hops > cap {
		return 0, fmt.Errorf("collect: related max_hops %d exceeds cap %d (MEDIAMON_RELATED_MAX_HOPS)", hops, cap)
	}
	return hops, nil
}

// resolveRelatedMaxExpansions resolves the request budget.
func resolveRelatedMaxExpansions(requested int) int {
	if requested > 0 {
		return requested
	}
	if n, ok := envInt("MEDIAMON_RELATED_MAX_EXPANSIONS"); ok && n > 0 {
		return n
	}
	return defaultRelatedMaxExpansions
}

// frontierNode is one queued expansion candidate.
type frontierNode struct {
	id  string
	hop int
}

// RelatedGraph collects the related-content graph of one seed item: the
// 1-hop adjacency by default, a budgeted K-hop breadth-first traversal
// when RelatedOptions.MaxHops > 1.
func (e *Engine) RelatedGraph(ctx context.Context, platform, seedID string, opt RelatedOptions) (RelatedGraph, error) {
	var g RelatedGraph
	g.SeedID = seedID
	hops, err := resolveRelatedMaxHops(opt.MaxHops)
	if err != nil {
		return g, err
	}
	maxExp := resolveRelatedMaxExpansions(opt.MaxExpansions)
	maxNodes := opt.MaxNodes
	if maxNodes <= 0 {
		maxNodes = maxExp*10 + 1 // every expansion returns at most 10 items
	}
	name, err := e.resolveName(platform, "related")
	if err != nil {
		return g, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return g, fmt.Errorf("collect: contract %q not registered", name)
	}
	bp, err := contracts.ParsePath(mainBindingRaw2(c))
	if err != nil {
		return g, err
	}
	idParam := firstPlaceholder(c, "aweme_id")
	paging := pacingFor(e.pacing, c.Paging.PageSleepMS)

	addNode := func(n RelatedNode) bool {
		if len(g.Nodes) >= maxNodes {
			return false
		}
		g.Nodes = append(g.Nodes, n)
		return true
	}
	known := map[string]bool{seedID: true}
	addNode(RelatedNode{AwemeID: seedID})
	var frontier []frontierNode

	expand := func(id string) ([]RelatedNode, error) {
		doc, ferr := e.Fetch(ctx, name, map[string]string{idParam: id}, nil)
		if ferr != nil {
			return nil, ferr
		}
		var nodes []RelatedNode
		for _, rec := range selectRecords(bp, doc) {
			it := bindItem(c, rec)
			if it.ID == "" {
				continue
			}
			nodes = append(nodes, RelatedNode{
				AwemeID:   it.ID,
				SecUID:    it.Author.SecUID,
				DiggCount: it.Stats.Digg,
			})
		}
		return nodes, nil
	}

	// Seed expansion (hop 1).
	first, err := expand(seedID)
	if err != nil {
		return g, err
	}
	g.Stats.Requests++
	for _, n := range first {
		g.Edges = append(g.Edges, RelatedEdge{Src: seedID, Dst: n.AwemeID})
		if known[n.AwemeID] {
			continue
		}
		known[n.AwemeID] = true
		if addNode(n) {
			frontier = append(frontier, frontierNode{id: n.AwemeID, hop: 1})
		} else {
			g.Stats.NodesCapped++
		}
	}
	g.Stats.HopsWalked = 1

	// K-hop expansion (hop >= 2), bounded by the expansion/node budgets.
	for len(frontier) > 0 && g.Stats.Requests < maxExp && hops >= 2 {
		cur := frontier[0]
		frontier = frontier[1:]
		if cur.hop+1 > hops {
			continue // deeper than K: stays a node, never expands
		}
		if ctx.Err() != nil {
			break
		}
		e.pageThink(ctx, paging)
		next, err := expand(cur.id)
		if err != nil {
			// Partial-data semantics: keep the graph walked so far, surface
			// the error only when nothing was collected.
			if len(g.Nodes) <= 1 {
				return g, err
			}
			break
		}
		g.Stats.Requests++
		if g.Stats.HopsWalked < cur.hop+1 {
			g.Stats.HopsWalked = cur.hop + 1
		}
		for _, n := range next {
			g.Edges = append(g.Edges, RelatedEdge{Src: cur.id, Dst: n.AwemeID})
			if known[n.AwemeID] {
				continue
			}
			known[n.AwemeID] = true
			if addNode(n) {
				frontier = append(frontier, frontierNode{id: n.AwemeID, hop: cur.hop + 1})
			} else {
				g.Stats.NodesCapped++
			}
		}
	}
	g.Stats.NNodes = len(g.Nodes)
	g.Stats.NEdges = len(g.Edges)
	if g.Stats.Requests > 0 {
		g.Stats.AvgOutDegree = float64(g.Stats.NEdges) / float64(g.Stats.Requests)
	}
	return g, nil
}

// mainBindingRaw2 returns the primary list binding path of a contract
// (readability alias at the related call site).
func mainBindingRaw2(c *contracts.Contract) string {
	_, raw := mainBindingRaw(c)
	return raw
}
