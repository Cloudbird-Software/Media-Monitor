// enrich.go — comment-author profile completion (the "payload + user-enrich"
// combination, validation-report capability #2 design gap).
//
// Corpus truth (goal_validation_report §2.2): the comment faces cannot carry
// the twelve-field author profile — douyin comment users have no
// signature/gender/counts, kuaishou comments only flat
// author_id/author_name/headurl, xhs only user_id/nickname/image. The profile
// contract (AGENTS.md / IR-MM-0001 AC-19) therefore requires combining the
// comment payload with the platform's user-enrichment face. After a comment
// walk completes, the engine enriches each UNIQUE author once through the
// platform's user contract and merges the result fill-forward (enrichment
// never overwrites payload-bound values; it only fills empty/zero fields).
//
// Silent discipline: enrich requests ride the exact same machinery as the
// walk itself — contract-declared URL/headers (browser header sets), the
// session cookie + UA binding via buildURL, and a human-paced think time
// between requests (pageThink, same log-normal distribution). No second,
// faster client exists for enrichment.
//
// Safety semantics:
//   - best-effort: an enrich failure NEVER fails the comment collection
//     (counted in obs as collect.comment_enrich_error and skipped);
//   - no rotation: enrichment issues single Fetches, never the auto-rotation
//     walk loop — a dead account surfaces on the comment pages themselves,
//     so enrichment cannot burn pool accounts;
//   - circuit breaker: three consecutive enrich failures (endpoint absent /
//     signer down) disable the rest of the pass instead of hammering;
//   - bounded fan-out: at most one request per unique author, optionally
//     capped by MEDIAMON_COMMENT_ENRICH_MAX;
//   - kill switch: MEDIAMON_COMMENT_ENRICH=0|off|false disables the pass.
package collect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// enrichPassState carries the per-collection-call enrichment bookkeeping.
type enrichPassState struct {
	seen     map[string]bool // enrich keys already completed/skipped
	fails    int             // consecutive failures (circuit breaker)
	enriched int
	skipped  int
	capped   int
	fetched  bool // any fetch issued yet (paces only between requests)
}

// commentEnrichEnabled resolves MEDIAMON_COMMENT_ENRICH (default ON).
func commentEnrichEnabled() bool {
	v := strings.TrimSpace(envString("MEDIAMON_COMMENT_ENRICH"))
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

// commentEnrichMax resolves MEDIAMON_COMMENT_ENRICH_MAX (0 = unlimited).
func commentEnrichMax() int {
	if n, ok := envInt("MEDIAMON_COMMENT_ENRICH_MAX"); ok && n > 0 {
		return n
	}
	return 0
}

// obsInc bumps an engine counter (nil-obs safe).
func (e *Engine) obsInc(name string, n int64) {
	if e.obs != nil {
		e.obs.Inc(name, n)
	}
}

// enrichCommenters completes the author profiles of cmts through the
// platform's user contract. Called by ItemComments / CommentReplies after a
// successful walk; errors are swallowed into obs counters by design.
func (e *Engine) enrichCommenters(ctx context.Context, platform string, cmts []model.Comment) {
	if len(cmts) == 0 || !commentEnrichEnabled() {
		return
	}
	name, err := e.resolveName(platform, "user")
	if err != nil {
		// No user face for this platform: nothing to combine (counted).
		e.obsInc("collect.comment_enrich_unavailable", 1)
		return
	}
	uc, ok := e.reg.Get(name)
	if !ok {
		e.obsInc("collect.comment_enrich_unavailable", 1)
		return
	}
	pacing := pacingFor(e.pacing, uc.Paging.PageSleepMS)
	st := &enrichPassState{seen: map[string]bool{}}
	maxKeys := commentEnrichMax()
	for i := range cmts {
		u := &cmts[i].User
		key := enrichKey(*u)
		if key == "" || st.seen[key] {
			continue
		}
		st.seen[key] = true
		if maxKeys > 0 && st.enriched >= maxKeys {
			st.capped++
			continue
		}
		if st.fails >= 3 {
			// Circuit breaker: the enrich face is absent or down; stop
			// hammering it for the rest of this collection.
			e.obsInc("collect.comment_enrich_circuit_open", 1)
			st.capped += len(cmts) - i
			break
		}
		if st.fetched {
			e.pageThink(ctx, pacing) // same human think time as page walks
		}
		st.fetched = true
		prof, err := e.fetchUserProfileByKey(ctx, name, key)
		if err != nil {
			if errors.Is(err, errContextDone) {
				return
			}
			st.fails++
			st.skipped++
			e.obsInc("collect.comment_enrich_error", 1)
			continue
		}
		st.fails = 0
		st.enriched++
		e.obsInc("collect.comment_enrich", 1)
		mergeUserProfile(u, prof)
	}
}

// errContextDone marks enrich fetches aborted by context cancellation.
var errContextDone = errors.New("context canceled")

// errContractNotFound / errNoUserRecord name the two non-transport enrich
// failure shapes (explicit, so callers and tests can distinguish them).
var (
	errContractNotFound = errors.New("collect: contract not registered")
	errNoUserRecord     = errors.New("collect: no user record bound")
)

// envString reads a trimmed string env var.
func envString(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// enrichKey picks the author identifier for the user face: sec_uid when the
// payload carries one (dy), else uid (ks author_id, xhs user_id — the synth
// user faces resolve both).
func enrichKey(u model.UserProfile) string {
	if strings.TrimSpace(u.SecUID) != "" {
		return u.SecUID
	}
	if strings.TrimSpace(u.UID) != "" {
		return u.UID
	}
	return ""
}

// fetchUserProfileByKey resolves one author profile with a SINGLE fetch
// (deliberately not the paginated walk: no auto-rotation loop, so enrich
// can never burn pool accounts — see package notes).
func (e *Engine) fetchUserProfileByKey(ctx context.Context, name, key string) (model.UserProfile, error) {
	c, ok := e.reg.Get(name)
	if !ok {
		return model.UserProfile{}, fmt.Errorf("collect %s: %w", name, errContractNotFound)
	}
	doc, err := e.Fetch(ctx, name, map[string]string{"sec_uid": key}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return model.UserProfile{}, errContextDone
		}
		return model.UserProfile{}, err
	}
	_, raw := mainBindingRaw(c)
	bp, perr := contracts.ParsePath(raw)
	if perr != nil {
		return model.UserProfile{}, perr
	}
	recs := selectRecords(bp, doc)
	if len(recs) == 0 {
		return model.UserProfile{}, fmt.Errorf("collect %s: %w", name, errNoUserRecord)
	}
	return bindUser(c, recs[0]), nil
}

// mergeUserProfile fills dst's empty/zero fields from src (fill-forward:
// payload-bound values always win; enrichment only completes the profile).
func mergeUserProfile(dst *model.UserProfile, src model.UserProfile) {
	if dst.UID == "" {
		dst.UID = src.UID
	}
	if dst.SecUID == "" {
		dst.SecUID = src.SecUID
	}
	if dst.ShortID == "" {
		dst.ShortID = src.ShortID
	}
	if dst.Nickname == "" {
		dst.Nickname = src.Nickname
	}
	if dst.AvatarURL == "" {
		dst.AvatarURL = src.AvatarURL
	}
	if dst.Signature == "" {
		dst.Signature = src.Signature
	}
	if dst.IPLabel == "" {
		dst.IPLabel = src.IPLabel
	}
	if dst.Gender == 0 {
		dst.Gender = src.Gender
	}
	if dst.FollowerCount == 0 {
		dst.FollowerCount = src.FollowerCount
	}
	if dst.FollowingCount == 0 {
		dst.FollowingCount = src.FollowingCount
	}
	if dst.AwemeCount == 0 {
		dst.AwemeCount = src.AwemeCount
	}
	if dst.TotalFavorited == 0 {
		dst.TotalFavorited = src.TotalFavorited
	}
	if len(src.Extra) > 0 {
		if dst.Extra == nil {
			dst.Extra = map[string]any{}
		}
		for k, v := range src.Extra {
			if _, exists := dst.Extra[k]; !exists {
				dst.Extra[k] = v
			}
		}
	}
}
