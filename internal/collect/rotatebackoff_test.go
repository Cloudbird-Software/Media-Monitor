package collect

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
)

// stampingHandler wraps a handler and records arrival timestamps (guarded by
// a mutex; httptest handlers run on multiple goroutines).
func stampingHandler(next http.Handler, stamps *[]time.Time) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*stamps = append(*stamps, time.Now())
		mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// TestRotationBackoffBeforeSwitch: after an auth wall the walk PAUSES before
// re-firing with the next account (report §4-A2: the old engine re-fired in
// 0-16ms; the human baseline stops to look at the error). With
// MEDIAMON_ROTATE_BACKOFF_MS=300 the post-401 gap must reach ~300ms·0.75+.
func TestRotationBackoffBeforeSwitch(t *testing.T) {
	t.Setenv("MEDIAMON_ROTATE_BACKOFF_MS", "300")
	env := newRotEnv(t)
	defer env.srv.Close()
	if err := env.pool.SetHealth("acc1", accounts.HealthHealthy, ""); err != nil {
		t.Fatal(err)
	}
	if err := env.pool.SetHealth("acc2", accounts.HealthDegraded, "seed"); err != nil {
		t.Fatal(err)
	}
	// Rebuild the engine with a request-timestamp recorder.
	var stamps []time.Time
	orig := env.srv.Config.Handler
	env.srv.Config.Handler = stampingHandler(orig, &stamps)

	_, _, err := env.eng.SearchItems(context.Background(), "douyin", "kw", "", model0Cursor(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stamps) < 3 {
		t.Fatalf("requests = %d, want >= 3 (401 + 2 pages)", len(stamps))
	}
	// stamps[0]=acc1 401, stamps[1]=acc2 page1: the gap is the rotation
	// backoff (>= 0.75·300ms = 225ms; jitter caps at 375ms).
	gap := stamps[1].Sub(stamps[0])
	if gap < 200*time.Millisecond {
		t.Fatalf("post-401 gap = %v, want >= ~225ms (backoff before switch)", gap)
	}
	if gap > 500*time.Millisecond {
		t.Fatalf("post-401 gap = %v, jitter band exceeded", gap)
	}
}

// TestRotationBackoffDisabled: MEDIAMON_ROTATE_BACKOFF_MS=0 restores the
// zero-delay rotation (emergency lever).
func TestRotationBackoffDisabled(t *testing.T) {
	t.Setenv("MEDIAMON_ROTATE_BACKOFF_MS", "0")
	env := newRotEnv(t)
	defer env.srv.Close()
	if err := env.pool.SetHealth("acc1", accounts.HealthHealthy, ""); err != nil {
		t.Fatal(err)
	}
	if err := env.pool.SetHealth("acc2", accounts.HealthDegraded, "seed"); err != nil {
		t.Fatal(err)
	}
	var stamps []time.Time
	orig := env.srv.Config.Handler
	env.srv.Config.Handler = stampingHandler(orig, &stamps)

	if _, _, err := env.eng.SearchItems(context.Background(), "douyin", "kw", "", model0Cursor(), 10); err != nil {
		t.Fatal(err)
	}
	gap := stamps[1].Sub(stamps[0])
	if gap > 150*time.Millisecond {
		t.Fatalf("disabled backoff still paused %v", gap)
	}
}
