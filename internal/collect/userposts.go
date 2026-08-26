// UserPosts — the account-history backtrack atom (IR-MM-0001 AC-6 /
// BEH-1/2): newest-first listing of one creator's posts through the
// platform's user_posts contract, with the optional backtrack predicate
// (window / min_engagement / stop-after-consecutive, predicate.go) and
// cursor resumption. The walk only lists — video download and comment
// collection stay separate atoms (BEH-2).
package collect

import (
	"context"
	"fmt"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// UserPosts lists one creator's post history (newest first). secUID is the
// platform-stable creator id (douyin sec_user_id / xhs user_id); it rides
// the contract's first placeholder. limit<=0 keeps the contract default.
func (e *Engine) UserPosts(ctx context.Context, platform, secUID string, cur model.Cursor, limit int, opt BacktrackOptions) ([]model.Item, model.Cursor, error) {
	name, err := e.resolveName(platform, "user_posts")
	if err != nil {
		return nil, cur, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return nil, cur, fmt.Errorf("collect: contract %q not registered", name)
	}
	pred, err := compileStopPredicate(opt, time.Now())
	if err != nil {
		return nil, cur, err
	}
	recs, nxt, err := e.fetchPagesWith(ctx, name,
		map[string]string{firstPlaceholder(c, "sec_uid"): secUID}, nil, cur, limit, pred)
	if err != nil {
		return nil, nxt, err
	}
	items := make([]model.Item, 0, len(recs))
	for _, r := range recs {
		items = append(items, bindItem(c, r))
	}
	return items, nxt, nil
}
