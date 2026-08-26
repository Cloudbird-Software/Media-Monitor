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

// walkState carries the consecutive-low counter across items.
type walkState struct {
	consecutiveLow int
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

// fetchPagesWith is fetchPages plus the optional backtrack predicate (see
// predicate.go). pred == nil keeps the legacy behavior byte-for-byte: the
// predicate application is skipped entirely and the loop is the original
// fetchPages logic.
func (e *Engine) fetchPagesWith(ctx context.Context, name string, pathParams, baseQuery map[string]string, cur model.Cursor, limit int, pred *stopPredicate) ([]map[string]any, model.Cursor, error) {
	c, ok := e.reg.Get(name)
	if !ok {
		return nil, cur, fmt.Errorf("collect: contract %q not registered", name)
	}
	_, raw := mainBindingRaw(c)
	if raw == "" {
		return nil, cur, fmt.Errorf("collect %s: no list binding (items/comments/users/members) declared", name)
	}
	bp, err := contracts.ParsePath(raw)
	if err != nil {
		return nil, cur, fmt.Errorf("collect %s: bad binding %q: %w", name, raw, err)
	}
	var out []map[string]any
	ccur := cur
	pages := 0
	st := &walkState{}
	for {
		if limit > 0 && len(out) >= limit {
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
		if c.Paging.CountParam != "" && limit > 0 {
			query[c.Paging.CountParam] = strconv.Itoa(limit)
		}
		doc, err := e.Fetch(ctx, name, pathParams, query)
		if err != nil {
			return nil, ccur, err
		}
		page := selectRecords(bp, doc)
		stop := false
		if pred != nil {
			page, stop = pred.apply(c, page, st)
		}
		out = append(out, page...)
		next := e.nextCursor(c, doc, ccur)
		pages++
		if stop || !next.HasMore || c.Paging.NextCursorPath == "" {
			return truncate(out, limit), next, nil
		}
		if pages >= maxPages {
			return nil, next, fmt.Errorf("collect %s: pagination exceeded %d pages (cursor did not settle)", name, maxPages)
		}
		ccur = next
	}
	return truncate(out, limit), ccur, nil
}
