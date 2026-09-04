package collect

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// EngagementFloor is one min_engagement clause: a metric
// (digg|comment|share|collect|play) and the threshold below which an item
// counts as low engagement.
type EngagementFloor struct {
	Metric    string `json:"metric"`
	Threshold int64  `json:"threshold"`
}

// BacktrackOptions carries the optional account-history backtrack predicate
// (IR-MM-0001 AC-4 / BEH-1..3). Zero value = no predicate: fetchPages
// behaves exactly as before. The predicate is a collect-time behavior and
// never enters the contract schema (IFACE-1).
type BacktrackOptions struct {
	// WindowMonths stops the walk once an item's create_time is older than
	// the cutoff; that item is not emitted.
	WindowMonths int
	// MinEngagement early-stops after StopAfterConsecutive consecutive
	// items below the threshold (IR D-6: creator history is not monotonic,
	// a single low item must not truncate).
	MinEngagement *EngagementFloor
	// StopAfterConsecutive defaults to DefaultStopConsecutive when 0.
	StopAfterConsecutive int
}

// DefaultStopConsecutive is the default N for the consecutive-low early
// stop (IR BEH-1: default 5).
const DefaultStopConsecutive = 5

// engagementMetricDefaults mirrors bindItem's stats fallback families per
// metric (play rides the play_count families).
var engagementMetricDefaults = map[string][]string{
	"digg": {
		"statistics.digg_count", "stats.digg_count", "digg_count", "like_count", "photo.like_count",
		"interact_info.liked_count",
	},
	"comment": {"statistics.comment_count", "stats.comment_count", "comment_count", "interact_info.comment_count"},
	"share":   {"statistics.share_count", "stats.share_count", "share_count", "interact_info.share_count"},
	"collect": {"statistics.collect_count", "stats.collect_count", "collect_count", "interact_info.collected_count"},
	"play":    {"statistics.play_count", "stats.play_count", "play_count", "video.play_count", "interact_info.play_count"},
}

// createTimeFallbacks mirrors bindItem's create_time resolution order.
var createTimeFallbacks = []string{
	"create_time", "aweme_info.create_time", "timestamp", "photo.timestamp", "note_card.create_time", "time",
}

// stopPredicate is the compiled backtrack predicate applied by fetchPages
// between pages/items. A nil *stopPredicate means no predicate at all.
type stopPredicate struct {
	windowCutoffUnix int64 // 0 = no window
	metric           string
	threshold        int64
	hasFloor         bool
	stopAfter        int
	now              time.Time
}

// compileStopPredicate validates options and compiles them. The zero
// BacktrackOptions compiles to a nil predicate (identical legacy behavior).
func compileStopPredicate(opt BacktrackOptions, now time.Time) (*stopPredicate, error) {
	if opt.WindowMonths == 0 && opt.MinEngagement == nil {
		return nil, nil
	}
	p := &stopPredicate{now: now}
	if opt.WindowMonths > 0 {
		p.windowCutoffUnix = now.AddDate(0, -opt.WindowMonths, 0).Unix()
	}
	if opt.MinEngagement != nil {
		switch opt.MinEngagement.Metric {
		case "digg", "comment", "share", "collect", "play":
		default:
			return nil, fmt.Errorf("collect: min_engagement.metric %q not in {digg,comment,share,collect,play}", opt.MinEngagement.Metric)
		}
		p.metric = opt.MinEngagement.Metric
		p.threshold = opt.MinEngagement.Threshold
		p.hasFloor = true
		p.stopAfter = opt.StopAfterConsecutive
		if p.stopAfter <= 0 {
			p.stopAfter = DefaultStopConsecutive
		}
	}
	return p, nil
}

// createTimeUnix resolves an item's create_time in unix seconds, accepting
// the millisecond form some platforms return (>1e12). ok=false when absent.
func createTimeUnix(c *contracts.Contract, rec map[string]any) (int64, bool) {
	v := fieldValue(c, "create_time", rec, createTimeFallbacks)
	if v == nil {
		return 0, false
	}
	n := asInt(v)
	if n <= 0 {
		return 0, false
	}
	if n > 1e12 { // millisecond timestamps (e.g. xhs note time)
		n /= 1000
	}
	return n, true
}

// engagementValue resolves the configured metric for one record. ok=false
// means the record carries no value for this metric (stats missing): the
// consecutive counter must stay neutral for it (IR BEH-3).
func (p *stopPredicate) engagementValue(c *contracts.Contract, rec map[string]any) (int64, bool) {
	v := fieldValue(c, "stats."+p.metric, rec, engagementMetricDefaults[p.metric])
	if v == nil {
		return 0, false
	}
	return asInt(v), true
}

// walkState carries the consecutive-low counter and the auto-rotation
// bookkeeping (tried account ids + rotation budget) across one walk.
type walkState struct {
	consecutiveLow int
	tried          map[string]bool
	rotations      int
}

// apply evaluates the predicate over one page of newly fetched records and
// returns the records to keep (emit) plus a stop signal. Rules:
//   - window: the first item older than the cutoff stops the walk and is
//     not emitted (nor is anything after it)
//   - floor: items below threshold count toward the consecutive stop; the
//     counter is neutral for items without the metric (neither reset nor
//     increment); hitting N consecutive lows stops the walk, low items
//     collected so far stay emitted (BEH-2: listing semantics — the
//     consumer filters by stats)
func (p *stopPredicate) apply(c *contracts.Contract, recs []map[string]any, st *walkState) ([]map[string]any, bool) {
	if p == nil {
		return recs, false
	}
	kept := recs[:0]
	for _, rec := range recs {
		if p.windowCutoffUnix > 0 {
			if ts, ok := createTimeUnix(c, rec); ok && ts < p.windowCutoffUnix {
				return kept, true
			}
		}
		kept = append(kept, rec)
		if p.hasFloor {
			if v, ok := p.engagementValue(c, rec); ok {
				if v < p.threshold {
					st.consecutiveLow++
					if st.consecutiveLow >= p.stopAfter {
						return kept, true
					}
				} else {
					st.consecutiveLow = 0
				}
			}
			// missing metric: neutral — counter unchanged
		}
	}
	return kept, false
}

// DefaultMaxCountPerRequest is the hard cap on the per-request count param
// (report item 3 / C1: the human baseline never asks for count=60/100 — dy
// stays at 20, occasional 50; a large count is a strong bot fingerprint).
const DefaultMaxCountPerRequest = 20

// maxCountPerRequest resolves the global count cap: MEDIAMON_MAX_COUNT
// overrides the default 20 (deployments behind tolerant gateways may raise
// it; values <= 0 fall back to the default).
func maxCountPerRequest() int {
	if n, ok := envInt("MEDIAMON_MAX_COUNT"); ok && n > 0 {
		return n
	}
	return DefaultMaxCountPerRequest
}

// pageCountFor computes the count param value for one page: the caller's
// limit (when set) clamped to the global per-request cap; when the caller
// sets no limit the contract's count_default applies (never zero — the cap
// is the final floor). The limit itself only stops the walk; it is NEVER
// passed through as a page size (t02: --limit 60 produced count=60 in a
// single request).
func pageCountFor(c *contracts.Contract, limit int) int {
	cap := maxCountPerRequest()
	want := 0
	if limit > 0 {
		want = limit
	} else if c.Paging.CountDefault > 0 {
		want = c.Paging.CountDefault
	}
	if want <= 0 || want > cap {
		return cap
	}
	return want
}

// maxPagesLimit resolves the pagination guard: MEDIAMON_MAX_PAGES overrides
// the built-in 100 (report item 8: 页数上限可配置).
func maxPagesLimit() int {
	if n, ok := envInt("MEDIAMON_MAX_PAGES"); ok && n > 0 {
		return n
	}
	return maxPages
}

// fetchPagesWith is fetchPages plus the optional backtrack predicate (see
// predicate.go). pred == nil keeps the legacy behavior byte-for-byte: the
// predicate application is skipped entirely and the loop is the original
// fetchPages logic.
func (e *Engine) fetchPagesWith(ctx context.Context, name string, pathParams, baseQuery map[string]string, cur model.Cursor, limit int, pred *stopPredicate) ([]map[string]any, model.Cursor, error) {
	o, err := e.fetchPagesCore(ctx, name, pathParams, baseQuery, cur, limit, pred, nil)
	return o.records, o.cursor, err
}

// walkOutcome is the full result of one pagination walk: the (deduped)
// records, the resume cursor, and the pagination-depth accounting the new
// aggregate atoms surface in their output schemas.
type walkOutcome struct {
	records []map[string]any
	cursor  model.Cursor
	pages   int // page fetches issued
	fetched int // records seen on the wire (before dedup)
	dupes   int // records dropped by the dedup key (0 without keyOf)
}

// fetchPagesDedup is fetchPagesWith plus record-level deduplication: keyOf
// extracts the identity of one raw record (e.g. aweme_id / user_id); records
// whose key was already emitted are dropped, and a page that yields ZERO new
// records stops the walk. This is the termination guard for cursor faces
// that rewind (dy aweme/post pre-fix shape, ks search/user's "1" loop):
// "dedupe by id + stop after a page with no new records" (capability-proposal
// A/B regression point).
func (e *Engine) fetchPagesDedup(ctx context.Context, name string, pathParams, baseQuery map[string]string, cur model.Cursor, limit int, keyOf func(map[string]any) string) (walkOutcome, error) {
	return e.fetchPagesCore(ctx, name, pathParams, baseQuery, cur, limit, nil, keyOf)
}

// fetchPagesCore is the shared pagination loop: cursor/count params per page
// (limit -> count), has_more driven continuation, final limit truncation,
// optional backtrack predicate and optional record dedup (keyOf != nil).
func (e *Engine) fetchPagesCore(ctx context.Context, name string, pathParams, baseQuery map[string]string, cur model.Cursor, limit int, pred *stopPredicate, keyOf func(map[string]any) string) (walkOutcome, error) {
	var out walkOutcome
	out.cursor = cur
	c, ok := e.reg.Get(name)
	if !ok {
		return out, fmt.Errorf("collect: contract %q not registered", name)
	}
	_, raw := mainBindingRaw(c)
	if raw == "" {
		return out, fmt.Errorf("collect %s: no list binding (items/comments/users/members) declared", name)
	}
	bp, err := contracts.ParsePath(raw)
	if err != nil {
		return out, fmt.Errorf("collect %s: bad binding %q: %w", name, raw, err)
	}
	var seen map[string]bool
	if keyOf != nil {
		seen = map[string]bool{}
	}
	ccur := cur
	st := &walkState{}
	paging := pacingFor(e.pacing, c.Paging.PageSleepMS)
	fe := e // fetch engine; rotated clones replace it under auto mode
	if fe.isAutoAccount() {
		bound, aerr := fe.bindInitial(name)
		if aerr != nil {
			return out, aerr
		}
		fe = bound
	}
	if st.tried == nil {
		st.tried = map[string]bool{}
	}
	for {
		if limit > 0 && len(out.records) >= limit {
			break
		}
		query := make(map[string]string, len(baseQuery)+2)
		for k, v := range baseQuery {
			query[k] = v
		}
		if c.Paging.CursorParam != "" && ccur.Source != nil {
			if v, ok := ccur.Source["cursor"]; ok && v != nil {
				query[c.Paging.CursorParam] = asStr(v)
			}
		}
		if c.Paging.CountParam != "" {
			// Count clamping (report C1): the count param is the PAGE SIZE,
			// never the caller's limit — always clamped to ≤ the per-request
			// cap (default 20, MEDIAMON_MAX_COUNT).
			query[c.Paging.CountParam] = strconv.Itoa(pageCountFor(c, limit))
		}
		doc, err := fe.Fetch(ctx, name, pathParams, query)
		if err != nil {
			if fe.isAutoAccount() && (errorsIs(err, ErrAuthWall) || errorsIs(err, ErrEmptyPage)) {
				// Human-shaped pause BEFORE switching (report A2: the old
				// code re-fired within 0-16ms of a 401/403).
				fe.sleepBeforeRotation(ctx, st.rotations)
				// Auto rotation (IR AC-9): auth wall / empty page switches
				// to the next health-ranked account and retries the SAME
				// page (cursor untouched — the walk never restarts).
				nfe, rerr := fe.rotateOn(name, err, st.tried, &st.rotations)
				if rerr != nil {
					out.records = truncate(out.records, limit)
					return out, rerr
				}
				fe = nfe
				continue
			}
			out.records = truncate(out.records, limit)
			return out, err
		}
		if id := fe.currentAccount(); id != "" && fe.accounts != nil {
			_ = fe.accounts.MarkSuccess(id)
		}
		page := selectRecords(bp, doc)
		out.fetched += len(page)
		stop := false
		if pred != nil {
			page, stop = pred.apply(c, page, st)
		}
		if keyOf != nil {
			// Dedup pass (capability proposals A/B): drop records whose key
			// was already emitted; a page with zero NEW records means the
			// cursor rewound — stop instead of re-fetching the same window.
			fresh := page[:0]
			for _, rec := range page {
				k := keyOf(rec)
				if k != "" && seen[k] {
					out.dupes++
					continue
				}
				if k != "" {
					seen[k] = true
				}
				fresh = append(fresh, rec)
			}
			page = fresh
			if out.pages > 0 && len(page) == 0 {
				stop = true
			}
		}
		out.records = append(out.records, page...)
		next := e.nextCursor(c, doc, ccur)
		out.pages++
		if stop || !next.HasMore || c.Paging.NextCursorPath == "" {
			out.records = truncate(out.records, limit)
			out.cursor = next
			return out, nil
		}
		if out.pages >= maxPagesLimit() {
			// Report item 8 / t03: hitting the page ceiling used to discard
			// ~1980 collected records and return nil. Instead: keep the data,
			// surface the live cursor so the caller can resume, and stop
			// cleanly (the guard exists against runaway cursors, not as a
			// reason to throw the walk away).
			if e.obs != nil {
				e.obs.Inc("collect.maxpages_hit", 1)
			}
			out.records = truncate(out.records, limit)
			out.cursor = next
			return out, nil
		}
		// Human pacing (report item 1/A1): think time between consecutive
		// pages, log-normal + clamped; disabled by MEDIAMON_EMERGENCY /
		// MEDIAMON_PAGE_SLEEP_MS=0 / per-contract paging.page_sleep_ms=-1.
		fe.pageThink(ctx, paging)
		ccur = next
	}
	out.records = truncate(out.records, limit)
	out.cursor = ccur
	return out, nil
}
