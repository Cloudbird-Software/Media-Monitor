// commentthread.go — CommentThreadProfile, the sub-comment full-chain +
// commenter-profile atom (capability proposal C, P1): for one item, produce
// the structured comment-section profile — the full top-level walk, the
// full 楼中楼 (nested replies) walk for roots that claim sub-comments, the
// commenter/commenter-of-replies profile join, and the comment-time
// distribution (burst rhythm).
//
// Design notes (atom boundary / schema trade-offs):
//   - The atom reuses the comments/replies CONTRACTS and the engine's CID
//     direct path (the replies contract's first placeholder carries only
//     the comment id — no item id needed mid-chain), but drives its own
//     walk through fetchPagesDedup so that (a) every root/reply cid is
//     deduped, (b) the pagination depth lands in the output schema, and
//     (c) ONE enrichment pass runs over roots+replies together instead of
//     per sub-walk.
//   - Commenter profiles ride the SAME enrich combination the comment atoms
//     use (enrich.go: single paced fetches, fill-forward, circuit breaker,
//     MEDIAMON_COMMENT_ENRICH kill switch, MEDIAMON_COMMENT_ENRICH_MAX
//     cap) — no second, faster face exists for this atom.
//   - PII: the per-commenter summary is minimized to identity + ip_label
//     (payload-side) + first timestamp + count; the full twelve-field
//     profile stays inside the engine, not in this output (proposal
//     decision question #3, pending human sign-off).
//   - timeseries delays are measured against the EARLIEST walked comment
//     (first_delay_h == 0 by construction; the probe baseline median
//     245.5h / span 8759.5h uses the same reference).
//   - ks sub-comments (graphql visionSubCommentList) have no contract and
//     no synth face — the atom fails closed through the existing
//     "replies not declared" path; dy/xhs are the live surfaces.
package collect

import (
	"context"
	"fmt"
	"sort"

	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// CommenterSummary is one unique commenter's thread footprint (minimized).
type CommenterSummary struct {
	SecUID         string `json:"sec_uid"`
	Nickname       string `json:"nickname"`
	ProfileHit     bool   `json:"profile_hit"`
	IPLabel        string `json:"ip_label,omitempty"`
	FirstCommentTs int64  `json:"first_comment_ts"`
	NComments      int    `json:"n_comments"`
}

// SubClosure is the claimed-vs-walked sub-comment accounting.
type SubClosure struct {
	RootsWalked int `json:"roots_walked"` // roots whose replies were walked
	Claimed     int `json:"claimed"`      // sum of claimed sub counts (reply_count)
	Walked      int `json:"walked"`       // replies actually collected
}

// CommentTimeseries is the comment-time distribution (hours, relative to
// the earliest walked comment).
type CommentTimeseries struct {
	FirstDelayH  float64 `json:"first_delay_h"`
	MedianDelayH float64 `json:"median_delay_h"`
	LastDelayH   float64 `json:"last_delay_h"`
}

// CommentThreadProfile is the structured comment-section profile of one
// item (schema per capability proposal C).
type CommentThreadProfile struct {
	ItemID          string             `json:"item_id"`
	NCommentsClaim  int64              `json:"n_comments_claim"`
	NCommentsWalked int                `json:"n_comments_walked"`
	Pages           int                `json:"pages"` // top-level pages fetched
	RootsWithSub    int                `json:"roots_with_sub"`
	SubClosure      SubClosure         `json:"sub_closure"`
	Commenters      []CommenterSummary `json:"commenters"`
	Timeseries      CommentTimeseries  `json:"timeseries"`
}

// CommentThreadOptions bounds the chain walk. Zero value: full walk.
type CommentThreadOptions struct {
	// RootLimit caps the top-level comments walked (0 = all).
	RootLimit int
	// SubRootLimit caps how many roots get their replies walked
	// (0 = every root that claims sub-comments).
	SubRootLimit int
	// SubLimit caps replies per root (0 = all).
	SubLimit int
}

// commenterFootprint accumulates one unique commenter's summary.
type commenterFootprint struct {
	sum CommenterSummary
}

// CommentThread builds the structured comment-section profile of one item:
// top-level walk → nested-reply walk for roots with sub-comments → one
// enrich pass over every unique commenter → aggregate statistics.
func (e *Engine) CommentThread(ctx context.Context, platform, itemID string, opt CommentThreadOptions) (CommentThreadProfile, error) {
	var out CommentThreadProfile
	out.ItemID = itemID

	commentsName, err := e.resolveName(platform, "comments")
	if err != nil {
		return out, err
	}
	cc, ok := e.reg.Get(commentsName)
	if !ok {
		return out, fmt.Errorf("collect: contract %q not registered", commentsName)
	}
	rootWalk, err := e.fetchPagesDedup(ctx, commentsName,
		map[string]string{firstPlaceholder(cc, "item_id"): itemID}, nil, model.Cursor{}, opt.RootLimit,
		func(rec map[string]any) string {
			return fieldStr(cc, "cid", rec, []string{"cid", "comment_id", "id", "commentId"})
		})
	if err != nil && len(rootWalk.records) == 0 {
		return out, err
	}
	roots := make([]model.Comment, 0, len(rootWalk.records))
	for _, rec := range rootWalk.records {
		cm := bindComment(cc, rec)
		if cm.AwemeID == "" {
			cm.AwemeID = itemID
		}
		if v, ok := cm.Extra["item_comment_total"]; ok {
			if n := asInt(v); n > out.NCommentsClaim {
				out.NCommentsClaim = n
			}
		}
		roots = append(roots, cm)
	}
	out.NCommentsWalked = len(roots)
	out.Pages = rootWalk.pages

	all := append([]model.Comment(nil), roots...)
	var subRoots []model.Comment
	for _, r := range roots {
		if r.ReplyCount > 0 {
			out.RootsWithSub++
			subRoots = append(subRoots, r)
		}
	}
	if len(subRoots) > 0 {
		repliesName, rerr := e.resolveName(platform, "replies")
		if rerr != nil {
			return out, rerr // roots walked, but the chain cannot close: surface it
		}
		rc, ok := e.reg.Get(repliesName)
		if !ok {
			return out, fmt.Errorf("collect: contract %q not registered", repliesName)
		}
		subCap := opt.SubRootLimit
		for _, root := range subRoots {
			if subCap > 0 && out.SubClosure.RootsWalked >= subCap {
				break
			}
			if ctx.Err() != nil {
				break
			}
			if out.SubClosure.RootsWalked > 0 {
				e.pageThink(ctx, pacingFor(e.pacing, rc.Paging.PageSleepMS))
			}
			subWalk, serr := e.fetchPagesDedup(ctx, repliesName,
				map[string]string{firstPlaceholder(rc, "comment_id"): root.CID}, nil, model.Cursor{}, opt.SubLimit,
				func(rec map[string]any) string {
					return fieldStr(rc, "cid", rec, []string{"cid", "comment_id", "id", "commentId"})
				})
			out.SubClosure.RootsWalked++
			out.SubClosure.Claimed += int(root.ReplyCount)
			if serr != nil && len(subWalk.records) == 0 {
				continue // partial-data semantics: keep the chain, note the gap
			}
			for _, rec := range subWalk.records {
				cm := bindComment(rc, rec)
				if cm.AwemeID == "" {
					cm.AwemeID = itemID
				}
				if cm.ReplyToCID == "" {
					cm.ReplyToCID = root.CID
				}
				all = append(all, cm)
			}
			out.SubClosure.Walked += len(subWalk.records)
		}
	}

	// ONE enrich pass over roots + replies (same combination machinery as
	// the comment atoms; env kill switch / cap / circuit breaker apply).
	e.enrichCommenters(ctx, platform, all)

	out.Commenters = summarizeCommenters(all)
	out.Timeseries = commentTimeseries(all)
	return out, nil
}

// summarizeCommenters folds the walked comments into per-unique-commenter
// footprints (identity key: sec_uid else uid), in first-seen order.
func summarizeCommenters(cmts []model.Comment) []CommenterSummary {
	var order []string
	byKey := map[string]*commenterFootprint{}
	for i := range cmts {
		u := cmts[i].User
		key := enrichKey(u)
		if key == "" {
			continue
		}
		fp, ok := byKey[key]
		if !ok {
			fp = &commenterFootprint{}
			fp.sum = CommenterSummary{
				SecUID:         key,
				Nickname:       u.Nickname,
				IPLabel:        u.IPLabel,
				FirstCommentTs: cmts[i].CreateTime,
				NComments:      1,
			}
			fp.sum.ProfileHit = profileFaceHit(u)
			byKey[key] = fp
			order = append(order, key)
			continue
		}
		fp.sum.NComments++
		if cmts[i].CreateTime < fp.sum.FirstCommentTs {
			fp.sum.FirstCommentTs = cmts[i].CreateTime
		}
		if fp.sum.Nickname == "" {
			fp.sum.Nickname = u.Nickname
		}
		if fp.sum.IPLabel == "" {
			fp.sum.IPLabel = u.IPLabel
		}
		if !fp.sum.ProfileHit {
			fp.sum.ProfileHit = profileFaceHit(u)
		}
	}
	out := make([]CommenterSummary, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k].sum)
	}
	return out
}

// commentTimeseries computes the delay distribution relative to the
// earliest walked comment (hours).
func commentTimeseries(cmts []model.Comment) CommentTimeseries {
	var ts []int64
	for i := range cmts {
		if cmts[i].CreateTime > 0 {
			ts = append(ts, cmts[i].CreateTime)
		}
	}
	if len(ts) == 0 {
		return CommentTimeseries{}
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	h := func(from, to int64) float64 { return float64(to-from) / 3600 }
	base := ts[0]
	med := 0.0
	if len(ts) > 1 {
		// median of the nonzero delays (== median over comments; zero only
		// for the base comment itself).
		delays := make([]float64, 0, len(ts)-1)
		for _, t := range ts[1:] {
			delays = append(delays, h(base, t))
		}
		sort.Float64s(delays)
		n := len(delays)
		if n%2 == 1 {
			med = delays[n/2]
		} else {
			med = (delays[n/2-1] + delays[n/2]) / 2
		}
	}
	return CommentTimeseries{
		FirstDelayH:  h(base, ts[0]),
		MedianDelayH: med,
		LastDelayH:   h(base, ts[len(ts)-1]),
	}
}
