// usersearch.go — UserSearch, the people-search / author-discovery atom
// (capability proposal B, P0): given a keyword, produce the candidate
// author list and optionally join each candidate's profile through the
// platform's profile faces — wiring the "discover → dossier (capability A)
// → track" chain.
//
// Design notes (atom boundary / schema trade-offs):
//   - ks-first: only kuaishou declares a user_search contract (synth oracle
//     implements neither dy query/user nor the xhs onebox face); the atom is
//     platform-generic and fails closed where no contract is declared.
//   - Termination: the corpus cursor rewinds to "1" at the window edge — the
//     walk dedupes by user_id and stops on a page with no new users
//     (fetchPagesDedup), so the pcursor loop is bounded by the distinct-user
//     window, not by the page guard.
//   - Profile join reuses the enrich discipline: single fetches (never the
//     auto-rotation walk), paced like page walks, fill-forward merge, a
//     three-failure circuit breaker, best-effort (a join failure never
//     fails the search).
package collect

import (
	"context"
	"errors"
	"fmt"

	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// UserSearchEntry is one discovered author candidate.
type UserSearchEntry struct {
	User       model.UserProfile `json:"user"`
	Verified   bool              `json:"verified"`
	Living     bool              `json:"living"`
	ProfileHit bool              `json:"profile_hit"` // profile join succeeded
}

// UserSearchOptions tunes the search walk and the profile join.
type UserSearchOptions struct {
	// JoinProfiles turns on the per-candidate profile join (default off).
	JoinProfiles bool
	// JoinLimit caps how many unique candidates get a profile join (0 = all).
	JoinLimit int
}

// userKeyFallbacks mirrors bindUser's uid resolution order (dedup key).
var userKeyFallbacks = []string{"user_id", "uid", "id", "sec_uid"}

// UserSearch collects people-search candidates for a keyword, paginated
// with user_id dedup, and optionally joins each candidate's profile.
func (e *Engine) UserSearch(ctx context.Context, platform, keyword string, cur model.Cursor, limit int, opt UserSearchOptions) ([]UserSearchEntry, model.Cursor, error) {
	name, err := e.resolveName(platform, "user_search")
	if err != nil {
		return nil, cur, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return nil, cur, fmt.Errorf("collect: contract %q not registered", name)
	}
	walk, err := e.fetchPagesDedup(ctx, name,
		map[string]string{querySlotParam(c): keyword}, nil, cur, limit,
		func(rec map[string]any) string {
			return fieldStr(c, "uid", rec, userKeyFallbacks)
		})
	if err != nil && len(walk.records) == 0 {
		return nil, walk.cursor, err
	}
	entries := make([]UserSearchEntry, 0, len(walk.records))
	for _, rec := range walk.records {
		u := bindUser(c, rec)
		entry := UserSearchEntry{User: u}
		if v, ok := u.Extra["verified"]; ok {
			entry.Verified = asBool(v)
		}
		if v, ok := u.Extra["living"]; ok {
			entry.Living = asBool(v)
		}
		entries = append(entries, entry)
	}
	if opt.JoinProfiles && len(entries) > 0 {
		e.joinUserProfiles(ctx, platform, entries, opt.JoinLimit)
	}
	return entries, walk.cursor, err
}

// joinUserProfiles completes each entry's profile through the platform's
// profile/user faces (single paced fetches, fill-forward merge, circuit
// breaker after three consecutive failures — enrich.go discipline).
func (e *Engine) joinUserProfiles(ctx context.Context, platform string, entries []UserSearchEntry, joinLimit int) {
	fails := 0
	joined := 0
	for i := range entries {
		if joinLimit > 0 && joined >= joinLimit {
			break
		}
		if fails >= 3 {
			e.obsInc("collect.user_search_join_circuit_open", 1)
			break
		}
		key := enrichKey(entries[i].User)
		if key == "" {
			continue
		}
		if joined > 0 {
			e.pageThink(ctx, e.joinPacing(platform))
		}
		prof, err := e.fetchAuthorProfile(ctx, platform, key)
		if err != nil {
			if errors.Is(err, errContextDone) || ctx.Err() != nil {
				return
			}
			fails++
			e.obsInc("collect.user_search_join_error", 1)
			continue
		}
		fails = 0
		joined++
		mergeUserProfile(&entries[i].User, prof)
		entries[i].ProfileHit = profileFaceHit(entries[i].User)
		e.obsInc("collect.user_search_join", 1)
	}
}

// joinPacing resolves the think time for the join loop from whichever user
// face the platform declares (contract page_sleep override included).
func (e *Engine) joinPacing(platform string) PacingConfig {
	if name, err := e.resolveName(platform, "user"); err == nil {
		if c, ok := e.reg.Get(name); ok {
			return pacingFor(e.pacing, c.Paging.PageSleepMS)
		}
	}
	return e.pacing
}

// profileFaceHit reports whether a joined profile carries evidence of a
// real record (a name plus at least one nonzero count field). Used as the
// join-coverage signal in the UserSearch/dossier outputs.
func profileFaceHit(u model.UserProfile) bool {
	if u.Nickname == "" && u.UID == "" && u.SecUID == "" {
		return false
	}
	return u.FollowerCount > 0 || u.FollowingCount > 0 || u.AwemeCount > 0 || u.TotalFavorited > 0
}
