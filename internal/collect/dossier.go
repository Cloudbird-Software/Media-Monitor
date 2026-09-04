// dossier.go — AuthorDossier, the author-profile aggregation atom
// (capability proposal A, P0): given one author id (dy sec_user_id /
// ks userId / xhs user_id), produce the aggregated dossier — the claimed
// profile face (self-reported metrics), the observed works backtrack
// (reusing the user_posts machinery), and the claimed-vs-observed
// consistency check.
//
// Design notes (atom boundary / schema trade-offs, recorded per the batch
// engineering requirement):
//   - The dossier is an ORCHESTRATION atom over existing faces, not a new
//     endpoint: dy profile/other + aweme/post, ks user face + profile/feed,
//     xhs user face + user_posted. Risk = the sum of the existing contracts;
//     no new signing family.
//   - claimed: the "profile" category (douyin-profile, corpus author chain)
//     when declared, else the "user" category (ks /api/user/info, xhs
//     user/info); the second face fills forward any fields the first left
//     empty (the same merge discipline as comment enrichment).
//   - observed: the user_posts walk with record-level dedup by item id and
//     stop-on-no-new-page (fetchPagesDedup) — the termination guard for
//     cursor faces that rewind (dy aweme/post pre-fix shape). The plain
//     UserPosts atom keeps its legacy behavior; the dossier needs the dedup
//     invariant asserted ("0 duplicates, bounded pages") in its output.
//   - consistency reports deltas, never verdicts: dy author-entity counts
//     and content counts are different sources (probe delta 17), so a hard
//     equality assertion would be wrong by construction.
//   - first_seen_in (discovery provenance: search|related|comment|
//     user_search) is caller context, not atom knowledge — deliberately
//     absent from the output schema.
package collect

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// DossierClaimed carries the self-reported author metrics.
type DossierClaimed struct {
	Nickname       string `json:"nickname"`
	FollowerCount  int64  `json:"follower_count"`
	TotalFavorited int64  `json:"total_favorited"`
	AwemeCount     int64  `json:"aweme_count"`
	Signature      string `json:"signature"`
}

// DossierObserved carries the measured works-backtrack statistics.
type DossierObserved struct {
	WorksWalked        int     `json:"works_walked"` // records fetched
	WorksUnique        int     `json:"works_unique"` // after id dedup
	Pages              int     `json:"pages"`        // page fetches issued
	SumDigg            int64   `json:"sum_digg"`
	MaxDigg            int64   `json:"max_digg"`
	MedianDigg         int64   `json:"median_digg"`
	PublishSpanDays    float64 `json:"publish_span_days"`
	MedianIntervalDays float64 `json:"median_interval_days"`
}

// DossierConsistency carries the claimed-vs-observed deltas.
type DossierConsistency struct {
	// CountDelta = claimed.aweme_count - observed.works_unique (dy author
	// entities and content rows are different sources; the delta itself is
	// the living data — probe baseline: nonzero on dy).
	CountDelta int64 `json:"count_delta"`
	// FavoritedVsSumDiggRatio = claimed.total_favorited / observed.sum_digg
	// (0 when either side is unavailable).
	FavoritedVsSumDiggRatio float64 `json:"favorited_vs_sum_digg_ratio"`
}

// AuthorDossier is the aggregated author profile (schema per capability
// proposal A; Profile additionally carries the full twelve-field user
// contract for downstream consumers).
type AuthorDossier struct {
	Site        string             `json:"site"`
	AuthorID    string             `json:"author_id"`
	AsOf        int64              `json:"as_of"`
	Profile     model.UserProfile  `json:"profile"` // full claimed face (joined)
	Claimed     DossierClaimed     `json:"claimed"`
	Observed    DossierObserved    `json:"observed"`
	Consistency DossierConsistency `json:"consistency"`
}

// DossierOptions tunes the observed walk. Zero value: unlimited walk
// (has_more-driven, maxPages-guarded).
type DossierOptions struct {
	// MaxWorks caps the unique works collected (the fetchPages limit).
	MaxWorks int
}

// AuthorDossier builds the aggregated profile of one author. The claimed
// face is fetched first (one or two paced single fetches), then the works
// walk runs through the platform's user_posts contract with id dedup.
func (e *Engine) AuthorDossier(ctx context.Context, platform, authorID string, opt DossierOptions) (AuthorDossier, error) {
	d := AuthorDossier{
		Site:     platform,
		AuthorID: authorID,
		AsOf:     time.Now().Unix(),
	}
	prof, err := e.fetchAuthorProfile(ctx, platform, authorID)
	if err != nil {
		return d, err
	}
	d.Profile = prof
	d.Claimed = DossierClaimed{
		Nickname:       prof.Nickname,
		FollowerCount:  prof.FollowerCount,
		TotalFavorited: prof.TotalFavorited,
		AwemeCount:     prof.AwemeCount,
		Signature:      prof.Signature,
	}

	name, err := e.resolveName(platform, "user_posts")
	if err != nil {
		return d, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return d, fmt.Errorf("collect: contract %q not registered", name)
	}
	idParam := firstPlaceholder(c, "sec_uid")
	walk, err := e.fetchPagesDedup(ctx, name,
		map[string]string{idParam: authorID}, nil, model.Cursor{}, opt.MaxWorks,
		func(rec map[string]any) string {
			return fieldStr(c, "id", rec, itemIDFallbacks)
		})
	if err != nil {
		return d, err // partial: keep claimed, surface the walk error
	}
	obs := &d.Observed
	obs.WorksWalked = walk.fetched
	obs.WorksUnique = len(walk.records)
	obs.Pages = walk.pages
	times := make([]int64, 0, len(walk.records))
	diggs := make([]int64, 0, len(walk.records))
	for _, rec := range walk.records {
		it := bindItem(c, rec)
		obs.SumDigg += it.Stats.Digg
		if it.Stats.Digg > obs.MaxDigg {
			obs.MaxDigg = it.Stats.Digg
		}
		diggs = append(diggs, it.Stats.Digg)
		if ts, ok := createTimeUnix(c, rec); ok {
			times = append(times, ts)
		}
	}
	obs.MedianDigg = medianInt64(diggs)
	obs.PublishSpanDays, obs.MedianIntervalDays = publishRhythm(times)

	d.Consistency = DossierConsistency{
		CountDelta: d.Claimed.AwemeCount - int64(obs.WorksUnique),
	}
	if obs.SumDigg > 0 && d.Claimed.TotalFavorited > 0 {
		d.Consistency.FavoritedVsSumDiggRatio = float64(d.Claimed.TotalFavorited) / float64(obs.SumDigg)
	}
	return d, nil
}

// itemIDFallbacks mirrors bindItem's id resolution order (used as the
// dedup key extractor for raw records).
var itemIDFallbacks = []string{
	"id", "aweme_id", "aweme_info.aweme_id", "photo.id", "note_id", "note_card.id", "noteCard.id", "collects_id",
}

// fetchAuthorProfile resolves one author's claimed face: the "profile"
// category contract when the platform declares one (douyin-profile), then
// the "user" category fill-forward (ks /api/user/info, xhs user/info).
// Errors only when neither face produces a record.
func (e *Engine) fetchAuthorProfile(ctx context.Context, platform, authorID string) (model.UserProfile, error) {
	var joined model.UserProfile
	got := false
	if name, err := e.resolveName(platform, "profile"); err == nil {
		if c, ok := e.reg.Get(name); ok {
			doc, ferr := e.Fetch(ctx, name, map[string]string{firstPlaceholder(c, "sec_uid"): authorID}, nil)
			if ferr != nil {
				if ctx.Err() != nil {
					return joined, ferr
				}
			} else if _, raw := mainBindingRaw(c); raw != "" {
				if bp, perr := contracts.ParsePath(raw); perr == nil {
					recs := selectRecords(bp, doc)
					if len(recs) > 0 {
						joined = bindUser(c, recs[0])
						got = true
					}
				}
			} else {
				// fields-only profile contract: the document is the record.
				var u model.UserProfile
				bindUserFrom(c, doc, nil, &u)
				if u.Nickname != "" || u.UID != "" || u.SecUID != "" {
					joined = u
					got = true
				}
			}
		}
	}
	if name, err := e.resolveName(platform, "user"); err == nil {
		uc, ok := e.reg.Get(name)
		if !ok {
			return joined, fmt.Errorf("collect: contract %q not registered", name)
		}
		pacing := pacingFor(e.pacing, uc.Paging.PageSleepMS)
		if got {
			e.pageThink(ctx, pacing) // same think time between the two faces
		}
		prof, uerr := e.fetchUserProfileByKey(ctx, name, authorID)
		if uerr == nil {
			if got {
				mergeUserProfile(&joined, prof)
			} else {
				joined = prof
				got = true
			}
		} else if !got {
			return joined, uerr
		}
	}
	if !got {
		return joined, fmt.Errorf("collect: no profile face declared for platform %q", platform)
	}
	return joined, nil
}

// medianInt64 returns the median of vs (0 when empty).
func medianInt64(vs []int64) int64 {
	if len(vs) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// publishRhythm computes the publish span (days) and the median interval
// between consecutive publishes (days) from unix-second timestamps.
func publishRhythm(timesSec []int64) (spanDays, medianIntervalDays float64) {
	if len(timesSec) == 0 {
		return 0, 0
	}
	sorted := append([]int64(nil), timesSec...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	spanDays = float64(sorted[len(sorted)-1]-sorted[0]) / 86400
	if len(sorted) < 2 {
		return spanDays, 0
	}
	intervals := make([]int64, 0, len(sorted)-1)
	for i := 1; i < len(sorted); i++ {
		if d := sorted[i] - sorted[i-1]; d > 0 {
			intervals = append(intervals, d)
		}
	}
	if len(intervals) == 0 {
		return spanDays, 0
	}
	medianIntervalDays = float64(medianInt64(intervals)) / 86400
	return spanDays, medianIntervalDays
}
