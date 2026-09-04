package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestServerFunc is a one-line httptest.Server builder.
func newTestServerFunc(h http.HandlerFunc) *httptest.Server { return httptest.NewServer(h) }

// TestBackoffJitterAndExponent: backoffFor grows exponentially and stays
// within the ±20% jitter band (fake randomness = sampled statistics).
func TestBackoffJitterAndExponent(t *testing.T) {
	c := New(Config{RetryBase: 100 * time.Millisecond})
	// attempt 1: base 100ms → band [80,120]
	lo, hi := time.Duration(0), time.Duration(0)
	for i := 0; i < 500; i++ {
		d := c.backoffFor(1, 0)
		if lo == 0 || d < lo {
			lo = d
		}
		if d > hi {
			hi = d
		}
	}
	if lo < 80*time.Millisecond || hi > 120*time.Millisecond {
		t.Fatalf("attempt1 band = [%v,%v], want within [80ms,120ms]", lo, hi)
	}
	if lo == hi {
		t.Fatalf("no jitter observed: %v", lo)
	}
	// attempt 3: base 400ms → band [320,480]
	lo, hi = 0, 0
	for i := 0; i < 500; i++ {
		d := c.backoffFor(3, 0)
		if lo == 0 || d < lo {
			lo = d
		}
		if d > hi {
			hi = d
		}
	}
	if lo < 320*time.Millisecond || hi > 480*time.Millisecond {
		t.Fatalf("attempt3 band = [%v,%v], want within [320ms,480ms]", lo, hi)
	}
	// cap at 30s
	if d := c.backoffFor(20, 0); d > 30*time.Second {
		t.Fatalf("cap violated: %v", d)
	}
}

// TestBackoffRetryAfterWins: a server Retry-After larger than the computed
// backoff raises the wait; a huge value is capped at 30s.
func TestBackoffRetryAfterWins(t *testing.T) {
	c := New(Config{RetryBase: 10 * time.Millisecond})
	if d := c.backoffFor(1, 5*time.Second); d != 5*time.Second {
		t.Fatalf("retry-after must win: %v", d)
	}
	if d := c.backoffFor(1, time.Hour); d != 30*time.Second {
		t.Fatalf("retry-after must be capped: %v", d)
	}
	if d := c.backoffFor(1, 10*time.Millisecond); d > 12*time.Millisecond {
		t.Fatalf("small retry-after must not inflate backoff: %v", d)
	}
}

// TestParseRetryAfter covers delta-seconds, HTTP-date, garbage and clamping.
func TestParseRetryAfter(t *testing.T) {
	now := time.Now()
	if d, ok := parseRetryAfter("2", now); !ok || d != 2*time.Second {
		t.Fatalf("seconds: %v %v", d, ok)
	}
	if d, ok := parseRetryAfter("", now); ok || d != 0 {
		t.Fatalf("empty: %v %v", d, ok)
	}
	if _, ok := parseRetryAfter("soon", now); ok {
		t.Fatal("garbage must not parse")
	}
	if d, ok := parseRetryAfter("-5", now); !ok || d != 0 {
		t.Fatalf("negative: %v %v", d, ok)
	}
	if d, ok := parseRetryAfter("3600", now); !ok || d != 30*time.Second {
		t.Fatalf("clamp: %v %v", d, ok)
	}
	future := now.Add(3 * time.Second)
	if d, ok := parseRetryAfter(future.Format(http.TimeFormat), now); !ok || d < 2900*time.Millisecond {
		t.Fatalf("http-date: %v %v", d, ok)
	}
}

// TestRetry429HonorsRetryAfter: the observable end-to-end behavior — with a
// Retry-After of 1s the second attempt arrives no earlier than ~1s.
func TestRetry429HonorsRetryAfter(t *testing.T) {
	var hits int
	done := make(chan time.Time, 1)
	srv := newTestServerFunc(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		done <- time.Now()
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	c := New(Config{RetryBase: 10 * time.Millisecond, MaxRetries: 2, Timeout: 5 * time.Second})
	start := time.Now()
	go func() { _, _, _ = c.Do(context.Background(), http.MethodGet, srv.URL, nil, nil) }()
	<-done
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("retry fired after %v, want >= ~1s (Retry-After)", elapsed)
	}
}

// TestMaxRetriesFromEnv: default 2, env override, invalid falls back.
func TestMaxRetriesFromEnv(t *testing.T) {
	t.Setenv("MEDIAMON_MAX_RETRIES", "")
	if n := MaxRetriesFromEnv(); n != 2 {
		t.Fatalf("default = %d, want 2", n)
	}
	t.Setenv("MEDIAMON_MAX_RETRIES", "5")
	if n := MaxRetriesFromEnv(); n != 5 {
		t.Fatalf("override = %d, want 5", n)
	}
	t.Setenv("MEDIAMON_MAX_RETRIES", "0")
	if n := MaxRetriesFromEnv(); n != 0 {
		t.Fatalf("explicit single-attempt = %d, want 0", n)
	}
	t.Setenv("MEDIAMON_MAX_RETRIES", "junk")
	if n := MaxRetriesFromEnv(); n != 2 {
		t.Fatalf("invalid = %d, want default 2", n)
	}
}
