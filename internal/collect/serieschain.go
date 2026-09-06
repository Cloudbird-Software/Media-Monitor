// serieschain.go — SeriesChain, the mix/series episode-chain atom
// (capability G / proposals H, P2): given a mix (curated collection) or
// series (short-drama) id, walk the full episode list — episode mapping,
// missing-episode detection and the update cursor for series-content
// completeness monitoring.
//
// Design notes (atom boundary / schema trade-offs):
//   - Two chain faces share one atom: mix carries the explicit
//     item_id_to_episode map (a JSON STRING on the envelope — a
//     document-level binding, hence this walk is hand-rolled like related.go
//     instead of riding fetchPages) and a plain index cursor; series rides a
//     max_cursor chain and has NO episode map, so episode numbers derive
//     from walk order (page position). EpisodeSource reports which.
//   - Termination guards mirror fetchPagesCore: has_more drives the walk,
//     records dedupe by item id (a zero-new page means the cursor rewound —
//     keep what we have and stop), the global max-pages guard applies
//     (MaxPages option overrides), and Limit truncates the episode count.
//   - Count discipline: the page size derives from the contract's
//     count_default (6, the corpus mix request shape) or the option,
//     clamped to the per-request cap (count-clamp spirit).
//   - Missing episodes are REPORTED, never guessed: the face has no claimed
//     total, so TotalEpisodes is the walked closure and MissingEps the gaps
//     inside the observed 1..max episode range (probe baselines walk closed
//     and gap-free: mix 13 episodes over 3 pages, series 33 over 6).
//   - An unknown/empty id is a clean empty success on this face (the synth
//     oracle answers an empty aweme_list with has_more=0), matching the
//     three-valued outcome discipline.
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// SeriesEpisode is one episode entry: its number and the bound item.
type SeriesEpisode struct {
	Episode int        `json:"ep"`
	Item    model.Item `json:"item"`
}

// SeriesChain is the walked episode list of one mix/series id.
type SeriesChain struct {
	Kind          string          `json:"kind"` // mix | series
	SeriesID      string          `json:"series_id"`
	TotalEpisodes int             `json:"total_episodes"`
	Episodes      []SeriesEpisode `json:"episodes"`
	// MissingEps lists gaps inside the observed 1..max episode range.
	MissingEps []int `json:"missing_eps"`
	// HasMore is true only when a budget (MaxPages/Limit) stopped the walk
	// before the face's has_more ended it; NextCursor resumes from there.
	HasMore    bool         `json:"has_more"`
	NextCursor model.Cursor `json:"next_cursor"`
	Pages      int          `json:"pages"`
	Walked     int          `json:"walked"` // records seen on the wire
	UniqueIDs  int          `json:"unique_ids"`
	Dupes      int          `json:"dupes"`
	// EpisodeSource names where episode numbers came from:
	// item_id_to_episode (mix map) | walk_order (series position).
	EpisodeSource string `json:"episode_source"`
}

// SeriesOptions bounds the chain walk.
type SeriesOptions struct {
	// Count is the page size (0 = contract count_default), clamped to the
	// per-request cap.
	Count int
	// Limit caps the episode count (0 = walk to the end).
	Limit int
	// MaxPages overrides the page guard (0 = the global max-pages guard).
	MaxPages int
	// Cursor resumes a budget-stopped walk (NextCursor of a previous walk).
	Cursor model.Cursor
}

// seriesChainKinds are the two declared chain faces.
var seriesChainKinds = map[string]bool{"mix": true, "series": true}

// SeriesChain walks the full episode chain of one mix/series id. kind is
// "mix" or "series" (the platform's declared chain contract category); the
// id is discovered passively from detail/search cards (mix_info.mix_id /
// series_info.series_id).
func (e *Engine) SeriesChain(ctx context.Context, platform, kind, seriesID string, opt SeriesOptions) (SeriesChain, error) {
	var out SeriesChain
	if !seriesChainKinds[kind] {
		return out, fmt.Errorf("collect: series chain kind %q not in {mix,series}", kind)
	}
	seriesID = strings.TrimSpace(seriesID)
	if seriesID == "" {
		return out, fmt.Errorf("collect: series chain: empty %s id", kind)
	}
	name, err := e.resolveName(platform, kind)
	if err != nil {
		return out, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return out, fmt.Errorf("collect: contract %q not registered", name)
	}
	_, raw := mainBindingRaw(c)
	if raw == "" {
		return out, fmt.Errorf("collect %s: no list binding declared", name)
	}
	bp, err := contracts.ParsePath(raw)
	if err != nil {
		return out, err
	}
	idParam := firstPlaceholder(c, kind+"_id")
	count := opt.Count
	if count <= 0 {
		count = c.Paging.CountDefault
	}
	if count <= 0 {
		count = maxCountPerRequest()
	}
	if count > maxCountPerRequest() {
		count = maxCountPerRequest()
	}
	if count < 1 {
		count = 1
	}
	pageGuard := opt.MaxPages
	if pageGuard <= 0 {
		pageGuard = maxPagesLimit()
	}
	out.Kind = kind
	out.SeriesID = seriesID
	out.EpisodeSource = "walk_order"
	hasMap := c.Binding.Fields["episode_map"] != ""
	if hasMap {
		out.EpisodeSource = "item_id_to_episode"
	}

	pacing := pacingFor(e.pacing, c.Paging.PageSleepMS)
	cur := opt.Cursor
	seen := map[string]bool{}
	epSeen := map[int]bool{}
	ordinal := 0
	stopped := false
	for !stopped {
		if opt.Limit > 0 && len(out.Episodes) >= opt.Limit {
			stopped = true
			break
		}
		if out.Pages >= pageGuard {
			if e.obs != nil {
				e.obs.Inc("collect.maxpages_hit", 1)
			}
			stopped = true
			break
		}
		query := map[string]string{}
		if c.Paging.CursorParam != "" && cur.Source != nil {
			if v, ok := cur.Source["cursor"]; ok && v != nil {
				query[c.Paging.CursorParam] = asStr(v)
			}
		}
		if c.Paging.CountParam != "" {
			query[c.Paging.CountParam] = strconv.Itoa(count)
		}
		doc, ferr := e.Fetch(ctx, name, map[string]string{idParam: seriesID}, query)
		if ferr != nil {
			// Partial-data semantics: keep the episodes walked so far,
			// surface the error only when nothing was collected.
			if len(out.Episodes) > 0 {
				out.NextCursor = cur
			}
			finalizeChain(&out, epSeen, false)
			return out, ferr
		}
		out.Pages++
		page := selectRecords(bp, doc)
		out.Walked += len(page)
		// Episode map (mix face): document-level JSON string id -> ep.
		epMap := map[string]int{}
		if hasMap {
			if s, ok := docField(c, "episode_map", doc).(string); ok && s != "" {
				var m map[string]any
				if json.Unmarshal([]byte(s), &m) == nil {
					for k, v := range m {
						epMap[k] = int(asInt(v))
					}
				}
			}
		}
		fresh := 0
		for _, rec := range page {
			it := bindItem(c, rec)
			if it.ID == "" {
				continue
			}
			if seen[it.ID] {
				out.Dupes++
				continue
			}
			seen[it.ID] = true
			ordinal++
			ep := ordinal
			if v, ok := epMap[it.ID]; ok && v > 0 {
				ep = v
			}
			if opt.Limit > 0 && len(out.Episodes) >= opt.Limit {
				break
			}
			out.Episodes = append(out.Episodes, SeriesEpisode{Episode: ep, Item: it})
			epSeen[ep] = true
			fresh++
		}
		next := e.nextCursor(c, doc, cur)
		if fresh == 0 && out.Pages > 1 {
			// Zero-new page: the cursor rewound — keep the chain, stop
			// (content exhausted; the face's has_more is a rewind artifact).
			out.NextCursor = next
			finalizeChain(&out, epSeen, false)
			return out, nil
		}
		cur = next
		if !next.HasMore || c.Paging.NextCursorPath == "" {
			out.NextCursor = next
			break
		}
		e.pageThink(ctx, pacing)
	}
	finalizeChain(&out, epSeen, stopped || cur.HasMore)
	return out, nil
}

// finalizeChain computes the derived totals of one walk: unique/episode
// counts, HasMore (budget-stopped or face-declared) and the missing-episode
// gaps inside the observed 1..max range.
func finalizeChain(out *SeriesChain, epSeen map[int]bool, hasMore bool) {
	out.HasMore = hasMore
	out.UniqueIDs = len(out.Episodes)
	out.TotalEpisodes = len(out.Episodes)
	maxEp := 0
	for ep := range epSeen {
		if ep > maxEp {
			maxEp = ep
		}
	}
	for ep := 1; ep <= maxEp; ep++ {
		if !epSeen[ep] {
			out.MissingEps = append(out.MissingEps, ep)
		}
	}
}

// sortedEpisodeNumbers returns the episode numbers in ascending order
// (e2e/test helper for the 1..N contiguity assertion).
func sortedEpisodeNumbers(eps []SeriesEpisode) []int {
	out := make([]int, 0, len(eps))
	for _, e := range eps {
		out = append(out, e.Episode)
	}
	sort.Ints(out)
	return out
}
