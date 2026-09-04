package datacenter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAddDedupAndCap(t *testing.T) {
	dir := t.TempDir()
	h, err := New(Config{Dir: dir, MaxRecords: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	r1 := Record{Platform: "douyin", UserKey: "u1", Nickname: "用户一"}
	r2 := Record{Platform: "douyin", UserKey: "u2", Nickname: "用户二"}
	r1dup := Record{Platform: "douyin", UserKey: "u1", Nickname: "重复"}

	if added, err := h.Add(r1); err != nil || !added {
		t.Fatalf("Add r1: added=%v err=%v", added, err)
	}
	if added, err := h.Add(r2); err != nil || !added {
		t.Fatalf("Add r2: added=%v err=%v", added, err)
	}
	if added, err := h.Add(r1dup); err != nil || added {
		t.Fatalf("Add dup: added=%v err=%v (want false)", added, err)
	}
	if all := h.List(nil, false); len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}

	// Add beyond cap: oldest (u1) should be evicted.
	if _, err := h.Add(Record{Platform: "douyin", UserKey: "u3", Nickname: "用户三"}); err != nil {
		t.Fatalf("Add u3: %v", err)
	}
	if _, err := h.Add(Record{Platform: "douyin", UserKey: "u4", Nickname: "用户四"}); err != nil {
		t.Fatalf("Add u4: %v", err)
	}
	all := h.List(nil, false)
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3 (cap)", len(all))
	}
	keys := map[string]bool{}
	for _, r := range all {
		keys[r.Key()] = true
	}
	if keys["douyin:u1"] {
		t.Fatal("oldest u1 should have been evicted")
	}
	if !keys["douyin:u4"] {
		t.Fatal("newest u4 should be present")
	}
}

func TestAddRequiresKey(t *testing.T) {
	dir := t.TempDir()
	h, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if added, err := h.Add(Record{Platform: "douyin"}); err != nil {
		t.Fatalf("Add: %v", err)
	} else if added {
		t.Fatal("record with no user_key should be rejected")
	}
}

func TestKeywordFilter(t *testing.T) {
	dir := t.TempDir()
	h, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	mustAdd := func(r Record) {
		t.Helper()
		if _, err := h.Add(r); err != nil {
			t.Fatalf("Add %s: %v", r.UserKey, err)
		}
	}
	mustAdd(Record{Platform: "douyin", UserKey: "a", Nickname: "露营爱好者", Payload: map[string]any{"text": "今天去爬山"}})
	mustAdd(Record{Platform: "douyin", UserKey: "b", Nickname: "美食博主", Payload: map[string]any{"text": "好吃的火锅"}})
	mustAdd(Record{Platform: "douyin", UserKey: "c", Nickname: "露营美食家", Payload: map[string]any{"text": "户外烹饪"}})

	// matchAny: "露营" matches a and c.
	got := h.List([]string{"露营"}, true)
	if len(got) != 2 {
		t.Fatalf("any-filter '露营' = %d, want 2", len(got))
	}
	// matchAll: "露营"+"美食" matches only c.
	got = h.List([]string{"露营", "美食"}, false)
	if len(got) != 1 || got[0].UserKey != "c" {
		t.Fatalf("all-filter = %+v, want [c]", got)
	}
}

// fakeClock is a manually-advanced clock for throttle tests (no real time).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestPushThrottleAndRetry(t *testing.T) {
	var pushes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	h, err := New(Config{Dir: dir, WebhookURL: srv.URL, WebhookMinInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	h.SetClock(clk.Now)
	if _, err := h.Add(Record{Platform: "douyin", UserKey: "x", Nickname: "测试"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := h.Push(context.Background()); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if pushes.Load() != 1 {
		t.Fatalf("pushes = %d, want 1", pushes.Load())
	}
	// Immediate second push should be throttled (no new request).
	if err := h.Push(context.Background()); err != nil {
		t.Fatalf("Push2: %v", err)
	}
	if pushes.Load() != 1 {
		t.Fatalf("pushes after throttle = %d, want 1", pushes.Load())
	}
	// Advancing past the min interval un-throttles (fake clock, zero real sleep).
	clk.Advance(101 * time.Millisecond)
	if err := h.Push(context.Background()); err != nil {
		t.Fatalf("Push3: %v", err)
	}
	if pushes.Load() != 2 {
		t.Fatalf("pushes after interval = %d, want 2", pushes.Load())
	}
}

// TestPushIfDueForcesFlushAtMaxInterval: once the silence since the last push
// reaches WebhookMaxInterval, PushIfDue flushes even though the min-interval
// throttle would still hold a plain Push.
func TestPushIfDueForcesFlushAtMaxInterval(t *testing.T) {
	var pushes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	h, err := New(Config{
		Dir:                dir,
		WebhookURL:         srv.URL,
		WebhookMinInterval: time.Hour, // plain Push would stay throttled for an hour
		WebhookMaxInterval: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	h.SetClock(clk.Now)
	if _, err := h.Add(Record{Platform: "douyin", UserKey: "m", Nickname: "上限"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// First push (never pushed before: due immediately).
	due, err := h.PushIfDue(context.Background())
	if err != nil || !due {
		t.Fatalf("first PushIfDue: due=%v err=%v", due, err)
	}
	if pushes.Load() != 1 {
		t.Fatalf("pushes = %d, want 1", pushes.Load())
	}
	// 5 minutes later: below max interval, not due.
	clk.Advance(5 * time.Minute)
	if due, err := h.PushIfDue(context.Background()); err != nil || due {
		t.Fatalf("early PushIfDue: due=%v err=%v (want false)", due, err)
	}
	if pushes.Load() != 1 {
		t.Fatalf("pushes = %d, want 1", pushes.Load())
	}
	// 6 more minutes: 11 > 10 max interval → forced flush despite the hour-long
	// min interval.
	clk.Advance(6 * time.Minute)
	due, err = h.PushIfDue(context.Background())
	if err != nil || !due {
		t.Fatalf("forced PushIfDue: due=%v err=%v", due, err)
	}
	if pushes.Load() != 2 {
		t.Fatalf("pushes after max interval = %d, want 2 (forced flush)", pushes.Load())
	}
	// No records → nothing to flush even when due.
	h2, err := New(Config{Dir: t.TempDir(), WebhookURL: srv.URL, WebhookMaxInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	h2.SetClock(clk.Now)
	if due, err := h2.PushIfDue(context.Background()); err != nil || due {
		t.Fatalf("empty hub PushIfDue: due=%v err=%v (want false)", due, err)
	}
}

func TestPushFailClosedQueuesRetry(t *testing.T) {
	var pushes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	h, err := New(Config{Dir: dir, WebhookURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Add(Record{Platform: "douyin", UserKey: "y", Nickname: "失败测试"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := h.Push(context.Background()); err == nil {
		t.Fatal("expected push error")
	}
	if pushes.Load() != 1 {
		t.Fatalf("pushes = %d, want 1", pushes.Load())
	}
	// Retry queue should hold the record; the endpoint is still failing, so
	// the record stays queued.
	outcome, err := h.RetryFailed(context.Background())
	if err != nil {
		t.Fatalf("RetryFailed: %v", err)
	}
	if outcome.Queued != 1 || outcome.Repushed != 0 || outcome.StillFailing != 1 {
		t.Fatalf("outcome = %+v, want queued=1 repushed=0 stillFailing=1", outcome)
	}
	if h.RetryQueueLen() != 1 {
		t.Fatalf("retry queue len = %d, want 1", h.RetryQueueLen())
	}
}

// TestPersistenceReadbackCrossProcess: the report-S2 defects — a second
// process over the same store dir must see the persisted rows (export CSV
//恒 0 行) and cross-process dedup must hold (re-adding a stored key is a
// duplicate, new keys append).
func TestPersistenceReadbackCrossProcess(t *testing.T) {
	dir := t.TempDir()
	h1, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := h1.Add(Record{Platform: "douyin", UserKey: fmt.Sprintf("u%d", i), Nickname: fmt.Sprintf("用户%d", i)}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := h1.Close(); err != nil {
		t.Fatal(err)
	}

	// Second process/hub over the same dir: rows read back.
	h2, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if all := h2.List(nil, false); len(all) != 5 {
		t.Fatalf("second process sees %d records, want 5 (export read path)", len(all))
	}
	// Cross-process dedup: stored key rejected, new key appended.
	if added, _ := h2.Add(Record{Platform: "douyin", UserKey: "u0", Nickname: "dup"}); added {
		t.Fatal("cross-process dedup failed: stored key re-added")
	}
	if added, err := h2.Add(Record{Platform: "douyin", UserKey: "u9", Nickname: "新"}); err != nil || !added {
		t.Fatalf("Add new key: added=%v err=%v", added, err)
	}
	if all := h2.List(nil, false); len(all) != 6 {
		t.Fatalf("records after cross-process add = %d, want 6", len(all))
	}

	// CSV export of the reloaded hub carries the persisted rows.
	var buf strings.Builder
	if err := WriteCSV(&buf, h2.List(nil, false), nil, false); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 7 { // header + 6 rows
		t.Fatalf("csv = %d lines, want 7", len(lines))
	}
}

// TestRetryQueueSuccessRemovesRows: records delivered by a retry pass leave
// the queue (atomically rewritten); still-failing ones stay. A later
// successful full Push clears the queue entirely (no full re-send).
func TestRetryQueueSuccessRemovesRows(t *testing.T) {
	var ok bool
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	h, err := New(Config{Dir: dir, WebhookURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, k := range []string{"a", "b"} {
		if _, err := h.Add(Record{Platform: "douyin", UserKey: k, Nickname: k}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := h.Push(context.Background()); err == nil {
		t.Fatal("expected push failure")
	}
	// Two consecutive failed pushes must not duplicate the queue.
	if err := h.Push(context.Background()); err == nil {
		t.Fatal("expected second push failure")
	}
	if h.RetryQueueLen() != 2 {
		t.Fatalf("retry queue len = %d, want 2 (enqueue dedup)", h.RetryQueueLen())
	}

	// Endpoint recovers: one retry pass delivers both and empties the queue.
	ok = true
	before := hits.Load()
	outcome, err := h.RetryFailed(context.Background())
	if err != nil {
		t.Fatalf("RetryFailed: %v", err)
	}
	if outcome.Queued != 2 || outcome.Repushed != 2 || outcome.StillFailing != 0 {
		t.Fatalf("outcome = %+v, want queued=2 repushed=2 failing=0", outcome)
	}
	if h.RetryQueueLen() != 0 {
		t.Fatalf("retry queue len after success = %d, want 0", h.RetryQueueLen())
	}
	// A second retry pass has nothing to do and sends nothing.
	outcome, err = h.RetryFailed(context.Background())
	if err != nil || outcome.Queued != 0 || hits.Load() != before+2 {
		t.Fatalf("second pass: outcome=%+v err=%v hits=%d", outcome, err, hits.Load())
	}
}

// TestRetryFailedWithoutWebhookIsObservable: retry without a configured
// webhook URL must fail explicitly (the CLI used to report "0 records still
// failing" while doing nothing).
func TestRetryFailedWithoutWebhookIsObservable(t *testing.T) {
	dir := t.TempDir()
	h, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Add(Record{Platform: "douyin", UserKey: "q", Nickname: "q"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.RetryFailed(context.Background()); err == nil {
		t.Fatal("RetryFailed without webhook URL must return an explicit error")
	} else if !errors.Is(err, ErrNoWebhook) {
		t.Fatalf("RetryFailed error = %v, want ErrNoWebhook", err)
	}
}

func TestCSVExport(t *testing.T) {
	dir := t.TempDir()
	h, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Add(Record{Platform: "douyin", UserKey: "c1", Nickname: "CSV用户", Timestamp: 1700000001, Payload: map[string]any{"text": "hi"}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	var buf strings.Builder
	if err := WriteCSV(&buf, h.List(nil, false), nil, false); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "platform,user_key,nickname") {
		t.Fatalf("csv header missing: %q", out)
	}
	if !strings.Contains(out, "CSV用户") {
		t.Fatalf("csv row missing: %q", out)
	}
	// Filtered export.
	buf.Reset()
	if err := WriteCSV(&buf, h.List(nil, false), []string{"不存在的词"}, true); err != nil {
		t.Fatalf("WriteCSV filtered: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 { // header only
		t.Fatalf("filtered csv = %d lines, want 1 (header)", len(lines))
	}
}
