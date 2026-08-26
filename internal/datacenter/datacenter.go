// Package datacenter is the platform-independent lead/engagement data hub:
// it accumulates collected records (comments, users, group members, live
// events), deduplicates by (platform, user_key), caps the queue (evicting the
// oldest), supports keyword filtering (any/all), and pushes to a configured
// webhook with retry. It consumes the output of the collect engine and the
// live monitor; it never reaches out to any platform itself.
package datacenter

import (
	"container/list"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

// Record is one normalized lead/engagement datum.
type Record struct {
	Platform  string         `json:"platform"`
	UserKey   string         `json:"user_key"` // unique within platform (uid/sec_uid)
	Nickname  string         `json:"nickname"`
	AvatarURL string         `json:"avatar_url"`
	Timestamp int64          `json:"timestamp"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// Key returns the dedup key (platform + user_key).
func (r Record) Key() string {
	return r.Platform + ":" + r.UserKey
}

// Hash returns a stable content hash (for change detection / retry dedup).
func (r Record) Hash() string {
	b, _ := json.Marshal([]any{r.Platform, r.UserKey, r.Nickname, r.Payload})
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

// Config tunes a Hub.
type Config struct {
	// Dir is the store directory for the dedup set + retry queue.
	Dir string
	// MaxRecords caps the in-memory + persisted queue; 0 = unlimited.
	MaxRecords int
	// WebhookURL is the optional push target. "" disables push.
	WebhookURL string
	// WebhookMinInterval throttles pushes to at most one per interval.
	WebhookMinInterval time.Duration
	// WebhookMaxInterval caps the silence between pushes: once this much time
	// has passed since the last successful push, PushIfDue forces a flush even
	// if the min-interval/batch conditions are not met. 0 disables the cap.
	WebhookMaxInterval time.Duration
	// HTTP is used for webhook POSTs (nil = default client).
	HTTP *httpclient.Client
}

// Hub accumulates, dedupes, filters and pushes records.
type Hub struct {
	cfg      Config
	mu       sync.Mutex
	records  *list.List               // oldest -> newest
	index    map[string]*list.Element // key -> element
	st       *store.Store
	http     *httpclient.Client
	lastPush time.Time
	pushed   bool
	now      func() time.Time // injectable clock (tests: no real time)
}

// New builds a Hub over a store dir. The store collection "datacenter" holds
// the records (one JSON row each); "datacenter_dedup" holds seen hashes;
// "datacenter_retry" holds failed pushes.
func New(cfg Config) (*Hub, error) {
	if cfg.Dir == "" {
		return nil, errors.New("datacenter: Dir is required")
	}
	st, err := store.Open(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("datacenter: open store: %w", err)
	}
	h := &Hub{
		cfg:     cfg,
		records: list.New(),
		index:   map[string]*list.Element{},
		st:      st,
		now:     time.Now,
	}
	if cfg.HTTP != nil {
		h.http = cfg.HTTP
	} else {
		h.http = httpclient.New(httpclient.Config{})
	}
	return h, nil
}

// SetClock overrides the hub clock (tests inject a fake; no real time).
func (h *Hub) SetClock(now func() time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = now
}

// Close flushes the underlying store.
func (h *Hub) Close() error {
	return h.st.Close()
}

// Add ingests a record: dedup by key, evict oldest if over cap, persist.
// Returns false if the record was a duplicate (not added).
func (h *Hub) Add(rec Record) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rec.Timestamp == 0 {
		rec.Timestamp = h.now().Unix()
	}
	if rec.UserKey == "" {
		return false, nil // fail-closed: no key = not a usable lead
	}
	if _, dup := h.index[rec.Key()]; dup {
		return false, nil
	}
	elem := h.records.PushBack(rec)
	h.index[rec.Key()] = elem
	if err := h.st.Append("datacenter", rec); err != nil {
		return false, fmt.Errorf("datacenter: persist: %w", err)
	}
	// Evict oldest beyond cap.
	if h.cfg.MaxRecords > 0 {
		for h.records.Len() > h.cfg.MaxRecords {
			old := h.records.Front()
			if old == nil {
				break
			}
			h.removeElement(old)
		}
	}
	return true, nil
}

func (h *Hub) removeElement(elem *list.Element) {
	rec := elem.Value.(Record)
	delete(h.index, rec.Key())
	h.records.Remove(elem)
}

// List returns records newest-first, optionally filtered by keywords.
// matchAny=true means a record matches if ANY keyword matches; false means ALL.
func (h *Hub) List(keywords []string, matchAny bool) []Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Record, 0, h.records.Len())
	for elem := h.records.Back(); elem != nil; elem = elem.Prev() {
		rec := elem.Value.(Record)
		if len(keywords) > 0 && !matchKeywords(rec, keywords, matchAny) {
			continue
		}
		out = append(out, rec)
	}
	return out
}

func matchKeywords(rec Record, keywords []string, matchAny bool) bool {
	text := strings.ToLower(rec.Nickname + " " + rec.Platform)
	if rec.Payload != nil {
		for _, v := range rec.Payload {
			text += " " + strings.ToLower(fmt.Sprint(v))
		}
	}
	hits := 0
	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		if strings.Contains(text, strings.ToLower(kw)) {
			if matchAny {
				return true
			}
			hits++
		}
	}
	if matchAny {
		return false
	}
	return hits == len(keywords)
}

// Push posts all records to the webhook URL (throttled by WebhookMinInterval).
// Fail-closed: failures are recorded to the retry queue, not dropped.
func (h *Hub) Push(ctx context.Context) error {
	return h.push(ctx, false)
}

// PushIfDue forces a flush when the silence since the last successful push has
// reached WebhookMaxInterval, even though the min-interval throttle would
// otherwise hold it. It reports whether a push was attempted.
func (h *Hub) PushIfDue(ctx context.Context) (bool, error) {
	if h.cfg.WebhookURL == "" || h.cfg.WebhookMaxInterval <= 0 {
		return false, nil
	}
	h.mu.Lock()
	due := !h.pushed || h.now().Sub(h.lastPush) >= h.cfg.WebhookMaxInterval
	h.mu.Unlock()
	if !due {
		return false, nil
	}
	h.mu.Lock()
	n := h.records.Len()
	h.mu.Unlock()
	if n == 0 {
		return false, nil
	}
	return true, h.push(ctx, true)
}

func (h *Hub) push(ctx context.Context, force bool) error {
	if h.cfg.WebhookURL == "" {
		return nil
	}
	if !force && h.cfg.WebhookMinInterval > 0 {
		h.mu.Lock()
		throttled := h.pushed && h.now().Sub(h.lastPush) < h.cfg.WebhookMinInterval
		h.mu.Unlock()
		if throttled {
			return nil
		}
	}
	records := h.List(nil, false)
	if len(records) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"records": records, "count": len(records)})
	if err != nil {
		return fmt.Errorf("datacenter: marshal push: %w", err)
	}
	_, _, perr := h.http.WithContract("datacenter_push").Do(ctx, "POST", h.cfg.WebhookURL, map[string]string{"Content-Type": "application/json"}, body)
	if perr != nil {
		h.enqueueRetry(records)
		return fmt.Errorf("datacenter: push failed (queued for retry): %w", perr)
	}
	h.mu.Lock()
	h.lastPush = h.now()
	h.pushed = true
	h.mu.Unlock()
	return nil
}

// TestWebhook POSTs a one-record probe to the webhook URL (探活).
func (h *Hub) TestWebhook(ctx context.Context) error {
	if h.cfg.WebhookURL == "" {
		return errors.New("datacenter: no webhook url configured")
	}
	probe, _ := json.Marshal(map[string]any{"type": "test", "time": time.Now().Unix()})
	_, _, err := h.http.WithContract("datacenter_test").Do(ctx, "POST", h.cfg.WebhookURL, map[string]string{"Content-Type": "application/json"}, probe)
	return err
}

func (h *Hub) enqueueRetry(records []Record) {
	for _, r := range records {
		_ = h.st.Append("datacenter_retry", r)
	}
}

// RetryFailed re-attempts pushing records from the retry queue. Returns the
// number still failing (re-enqueued).
func (h *Hub) RetryFailed(ctx context.Context) (int, error) {
	if h.cfg.WebhookURL == "" {
		return 0, nil
	}
	var failing []Record
	var stillFail int
	_ = h.st.Scan("datacenter_retry", func(raw []byte) error {
		var r Record
		if err := json.Unmarshal(raw, &r); err == nil {
			failing = append(failing, r)
		}
		return nil
	})
	if len(failing) == 0 {
		return 0, nil
	}
	for _, r := range failing {
		body, err := json.Marshal(map[string]any{"records": []Record{r}})
		if err != nil {
			stillFail++
			_ = h.st.Append("datacenter_retry", r)
			continue
		}
		_, _, perr := h.http.WithContract("datacenter_retry").Do(ctx, "POST", h.cfg.WebhookURL, map[string]string{"Content-Type": "application/json"}, body)
		if perr != nil {
			stillFail++
			_ = h.st.Append("datacenter_retry", r)
		}
	}
	return stillFail, nil
}
