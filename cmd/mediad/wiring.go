// wiring.go holds the mediad startup wiring that composes internal packages
// at the cmd layer: the account-pool/UA-pool injection, the
// datacenter hub (webhook push loop + final flush) and the IM unread poller.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/datacenter"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// ---- environment conventions ----

// accountsDirEnv resolves the account pool dir: MEDIAMON_ACCOUNTS_DIR wins,
// default <dataDir>/accounts (same style as the MEDIAMON_*_COOKIES overrides).
func accountsDirEnv(dataDir string) string {
	if d := os.Getenv("MEDIAMON_ACCOUNTS_DIR"); d != "" {
		return d
	}
	return filepath.Join(dataDir, "accounts")
}

// uaPoolUserAgents loads the shared UA rotation pool via
// accounts.LoadUAPoolDefault (MEDIAMON_UA_POOL overrides the path; default is
// the executable-relative data/ua-pool.json). A missing/broken file is NOT an
// error: the UA pool is an enhancement, not a gate — nil keeps the HTTP
// client's built-in pool.
func uaPoolUserAgents() []string {
	pool, err := accounts.LoadUAPoolDefault(os.Getenv("MEDIAMON_UA_POOL"))
	if err != nil || pool == nil {
		return nil
	}
	path := os.Getenv("MEDIAMON_UA_POOL")
	if path == "" {
		if path, err = accounts.DefaultUAPoolPath(); err != nil {
			return nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		UAs []string `json:"uas"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.UAs) == 0 {
		return nil
	}
	log.Printf("ua pool: %d user-agents loaded from %s", len(doc.UAs), path)
	return doc.UAs
}

// ---- datacenter hub ----

// envDuration parses a duration env var ("30s", "5m"); unset/invalid → 0.
func envDuration(key string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("warn: %s=%q is not a duration: %v (ignored)", key, v, err)
		return 0
	}
	return d
}

// wireDatacenter opens the datacenter hub (MEDIAMON_DATACENTER_DIR override,
// default <dataDir>/datacenter) and applies the webhook env config. Without
// MEDIAMON_WEBHOOK_URL the push side stays silently off while aggregation
// keeps working.
func (d *daemon) wireDatacenter(dataDir string) {
	dir := os.Getenv("MEDIAMON_DATACENTER_DIR")
	if dir == "" {
		dir = filepath.Join(dataDir, "datacenter")
	}
	cfg := datacenter.Config{
		Dir:                dir,
		WebhookURL:         os.Getenv("MEDIAMON_WEBHOOK_URL"),
		WebhookMinInterval: envDuration("MEDIAMON_WEBHOOK_MIN_INTERVAL"),
		WebhookMaxInterval: envDuration("MEDIAMON_WEBHOOK_MAX_INTERVAL"),
	}
	h, err := datacenter.New(cfg)
	if err != nil {
		log.Printf("warn: datacenter hub unavailable: %v", err)
		return
	}
	d.hub = h
	if cfg.WebhookURL == "" {
		d.webhookDesc = "push disabled (MEDIAMON_WEBHOOK_URL unset); aggregation still active"
	} else {
		d.webhookDesc = fmt.Sprintf("%s (min=%s max=%s)", cfg.WebhookURL, cfg.WebhookMinInterval, cfg.WebhookMaxInterval)
	}
}

// defaultPushInterval is how often the background loop calls PushIfDue (the
// max-interval enforcement only fires on these ticks).
const defaultPushInterval = 30 * time.Second

// startPushLoop periodically calls Hub.PushIfDue so the max-interval cap
// forces a flush even when the min-interval throttle holds regular pushes.
func (d *daemon) startPushLoop(ctx context.Context) {
	if d.hub == nil {
		return
	}
	iv := d.pushInterval
	if iv <= 0 {
		iv = defaultPushInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := d.hub.PushIfDue(ctx); err != nil {
				d.counters.Inc("datacenter.push_error", 1)
			}
		}
	}
}

// finalFlush is the shutdown path: force any due push, then a best-effort
// regular push (min-interval throttled), then close the hub store.
func (d *daemon) finalFlush(ctx context.Context) {
	if d.hub == nil {
		return
	}
	if _, err := d.hub.PushIfDue(ctx); err != nil {
		log.Printf("warn: datacenter final push-if-due: %v", err)
	}
	if err := d.hub.Push(ctx); err != nil {
		log.Printf("warn: datacenter final push: %v", err)
	}
	if err := d.hub.Close(); err != nil {
		log.Printf("warn: datacenter close: %v", err)
	}
}

// hubAdd ingests records into the hub; dedup/caps are the library's job.
// Counters feed the dashboard's aggregation statistics.
func (d *daemon) hubAdd(recs ...datacenter.Record) {
	if d.hub == nil {
		return
	}
	for _, r := range recs {
		ok, err := d.hub.Add(r)
		if err != nil {
			d.counters.Inc("datacenter.add_error", 1)
			continue
		}
		d.dcIngest.Add(1)
		if ok {
			d.dcAdded.Add(1)
		}
	}
}

// ---- record mappers (collect output -> datacenter records) ----

func profileKey(u model.UserProfile) string {
	if u.SecUID != "" {
		return u.SecUID
	}
	return u.UID
}

func itemRecords(platform string, items []model.Item) []datacenter.Record {
	var out []datacenter.Record
	for _, it := range items {
		key := profileKey(it.Author)
		if key == "" {
			continue
		}
		out = append(out, datacenter.Record{
			Platform:  platform,
			UserKey:   key,
			Nickname:  it.Author.Nickname,
			AvatarURL: it.Author.AvatarURL,
			Timestamp: it.CreateTime,
			Payload:   map[string]any{"kind": "item", "item_id": it.ID, "desc": it.Desc, "media_type": it.MediaType},
		})
	}
	return out
}

func commentRecords(platform string, cmts []model.Comment) []datacenter.Record {
	var out []datacenter.Record
	for _, cm := range cmts {
		key := profileKey(cm.User)
		if key == "" {
			continue
		}
		out = append(out, datacenter.Record{
			Platform:  platform,
			UserKey:   key,
			Nickname:  cm.User.Nickname,
			AvatarURL: cm.User.AvatarURL,
			Timestamp: cm.CreateTime,
			Payload:   map[string]any{"kind": "comment", "cid": cm.CID, "aweme_id": cm.AwemeID, "text": cm.Text},
		})
	}
	return out
}

func profileRecord(platform string, u model.UserProfile, kind string) []datacenter.Record {
	key := profileKey(u)
	if key == "" {
		return nil
	}
	return []datacenter.Record{{
		Platform:  platform,
		UserKey:   key,
		Nickname:  u.Nickname,
		AvatarURL: u.AvatarURL,
		Payload:   map[string]any{"kind": kind, "uid": u.UID, "follower_count": u.FollowerCount},
	}}
}

func memberRecords(platform string, members []model.GroupMember) []datacenter.Record {
	var out []datacenter.Record
	for _, m := range members {
		key := profileKey(m.UserProfile)
		if key == "" {
			continue
		}
		out = append(out, datacenter.Record{
			Platform:  platform,
			UserKey:   key,
			Nickname:  m.Nickname,
			AvatarURL: m.AvatarURL,
			Payload:   map[string]any{"kind": "group_member", "group_id": m.GroupID, "uid": m.UID},
		})
	}
	return out
}

// ---- IM unread poller ----

// imStatus is the dashboard-visible snapshot of one account's last poll.
type imStatus struct {
	Platform      string `json:"platform"`
	AccountID     string `json:"account_id,omitempty"`
	TotalUnread   int64  `json:"total_unread"`
	Conversations int    `json:"conversations"`
	LastPoll      int64  `json:"last_poll"`
	Error         string `json:"error,omitempty"`
}

// imPoller keeps the most recent IM-unread status per (platform, account).
type imPoller struct {
	mu sync.Mutex
	by map[string]imStatus
}

func newIMPoller() *imPoller { return &imPoller{by: map[string]imStatus{}} }

func (p *imPoller) update(s imStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.by[s.Platform+"/"+s.AccountID] = s
}

func (p *imPoller) snapshot() []imStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]imStatus, 0, len(p.by))
	for _, s := range p.by {
		out = append(out, s)
	}
	// Deterministic order for the dashboard/tests.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j-1].Platform+"/"+out[j-1].AccountID) > (out[j].Platform+"/"+out[j].AccountID); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// pollIMUnreadOnce executes one IM-unread poll: the result lands in the
// dashboard status map and (deduped by the hub) in the datacenter. Poll
// errors are recorded in the status instead of failing the polling task.
func (d *daemon) pollIMUnreadOnce(ctx context.Context, platform, accountID string) {
	eng := d.engineFor(accountID)
	if eng == nil {
		return
	}
	st := imStatus{Platform: platform, AccountID: accountID, LastPoll: time.Now().Unix()}
	res, err := eng.FetchIMUnread(ctx, platform)
	if err != nil {
		st.Error = err.Error()
	} else {
		st.TotalUnread = res.TotalUnread
		st.Conversations = len(res.Conversations)
		key := accountID
		if key == "" {
			key = "default"
		}
		d.hubAdd(datacenter.Record{
			Platform: platform,
			UserKey:  key,
			Payload:  map[string]any{"kind": "im_unread", "total_unread": res.TotalUnread, "conversations": st.Conversations},
		})
	}
	d.im.update(st)
}

// startIMPoll drives an "im-unread-poll" task submitted via the tasks API:
// one poll per interval (config: platform, account_id, interval_seconds)
// until the daemon context ends. The task runner owns the state transitions
// (running -> cancelled on shutdown); poll failures land in the status map.
func (d *daemon) startIMPoll(task model.Task) {
	platform := strVal(task.Config, "platform")
	accountID := strVal(task.Config, "account_id")
	interval := time.Duration(intVal(task.Config, "interval_seconds", 60)) * time.Second
	if platform == "" || d.engine == nil || d.ctx == nil || interval <= 0 {
		return
	}
	go func() {
		_ = d.runner.Run(d.ctx, task.ID, func(ctx context.Context, cur model.Cursor) (model.Cursor, error) {
			d.pollIMUnreadOnce(ctx, platform, accountID)
			select {
			case <-ctx.Done():
				return cur, ctx.Err()
			case <-time.After(interval):
			}
			return model.Cursor{Page: cur.Page + 1, HasMore: true}, nil
		})
	}()
}

// dcStats exposes the aggregation counters for tests and the dashboard.
type dcStats struct {
	Ingested int64 `json:"ingested"` // records offered to the hub
	Added    int64 `json:"added"`    // records that survived dedup
	Stored   int   `json:"stored"`   // records currently held (post-cap)
}

func (d *daemon) datacenterStats() dcStats {
	s := dcStats{Ingested: d.dcIngest.Load(), Added: d.dcAdded.Load()}
	if d.hub != nil {
		s.Stored = len(d.hub.List(nil, false))
	}
	return s
}
