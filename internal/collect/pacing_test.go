package collect

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

// TestLognormalSleepDistribution verifies the sampled think-time shape:
// median near the configured μ, p90/p99 heavy tail in the expected band for
// σ=1.0, and the [Min,Max] clamp honored (fake randomness = seeded RNG).
func TestLognormalSleepDistribution(t *testing.T) {
	p := PacingConfig{Enabled: true, Median: 1500 * time.Millisecond, Sigma: 1.0,
		Min: 250 * time.Millisecond, Max: 30 * time.Second}
	rnd := rand.New(rand.NewSource(42))
	const n = 20000
	samples := make([]float64, n)
	for i := range samples {
		samples[i] = float64(p.lognormalSleep(rnd)) / float64(time.Millisecond)
	}
	sort.Float64s(samples)
	q := func(f float64) float64 { return samples[int(f*float64(n))] }
	p50, p90, p99, mx := q(0.50), q(0.90), q(0.99), samples[n-1]
	t.Logf("p50=%.0fms p90=%.0fms p99=%.0fms max=%.0fms", p50, p90, p99, mx)
	// median: ±35% of 1500ms (log-normal sample median converges to μ)
	if p50 < 1000 || p50 > 2000 {
		t.Fatalf("p50 out of band: %.0fms", p50)
	}
	// σ=1.0 → p90 = μ·e^1.2816 ≈ 3.6·μ; accept 2.5x-5x
	if p90 < 2.5*1500 || p90 > 5*1500 {
		t.Fatalf("p90 out of band for sigma=1.0: %.0fms", p90)
	}
	// heavy tail must reach well past 8s at p99 (report: tail 8s+)
	if p99 < 8000 {
		t.Fatalf("p99 tail too light: %.0fms", p99)
	}
	if mx > 30*1000+1 {
		t.Fatalf("max clamp violated: %.0fms", mx)
	}
}

// TestLognormalSleepClampMin verifies the Min floor.
func TestLognormalSleepClampMin(t *testing.T) {
	p := PacingConfig{Enabled: true, Median: 1500 * time.Millisecond, Sigma: 1.0,
		Min: 1200 * time.Millisecond, Max: 30 * time.Second}
	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < 2000; i++ {
		if d := p.lognormalSleep(rnd); d < 1200*time.Millisecond {
			t.Fatalf("sample below Min floor: %v", d)
		}
	}
}

// TestPacingDisabledConfigs verifies all kill-switches return zero delay.
func TestPacingDisabledConfigs(t *testing.T) {
	cases := []PacingConfig{
		{Enabled: false, Median: time.Second, Sigma: 1, Min: time.Millisecond, Max: time.Second},
		{Enabled: true, Median: 0, Sigma: 1, Min: time.Millisecond, Max: time.Second},
		{Enabled: true, Median: time.Second, Sigma: 0, Min: time.Millisecond, Max: time.Second},
		{Enabled: true, Median: time.Second, Sigma: 1, Min: time.Millisecond, Max: 0},
	}
	rnd := rand.New(rand.NewSource(1))
	for i, p := range cases {
		if d := p.lognormalSleep(rnd); d != 0 {
			t.Fatalf("case %d: expected 0, got %v", i, d)
		}
	}
}

// TestPacingFromEnvKillSwitches covers the emergency-mode escape hatches.
func TestPacingFromEnvKillSwitches(t *testing.T) {
	t.Setenv("MEDIAMON_EMERGENCY", "1")
	if p := PacingFromEnv(); p.Enabled {
		t.Fatal("MEDIAMON_EMERGENCY=1 must disable pacing")
	}
}

// TestPacingBetweenPages drives a real pagination walk against a loopback
// mock with a deterministic sleep hook (fake clock): exactly pages-1 sleeps,
// none before page 1, none after the final page, and zero sleeps when the
// pacing engine is built with pacing disabled (legacy behavior).
func TestPacingBetweenPages(t *testing.T) {
	pagesServed := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pagesServed++
		cursor := r.URL.Query().Get("cursor")
		next := map[string]string{"": "p2", "p2": "p3", "p3": ""}[cursor]
		w.Header().Set("Content-Type", "application/json")
		if next == "" {
			w.Write([]byte(`{"data":[{"id":"x"}],"has_more":false}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"x"}],"has_more":true,"cursor":"` + next + `"}`))
	}))
	defer srv.Close()
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "pacing-search", Platform: "mock", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/list", Method: "GET"},
		Binding:   contracts.Binding{Items: "$.data"},
		Paging:    contracts.Paging{CursorParam: "cursor", HasMorePath: "$.has_more", NextCursorPath: "$.cursor", CountParam: "count", CountDefault: 20},
	})

	newEngine := func(pacing *PacingConfig) (*Engine, *[]time.Duration) {
		var sleeps []time.Duration
		e := New(Context{
			Registry: reg,
			HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
			Obs:      obs.NewCounterMap(),
			Pacing:   pacing,
		})
		e.sleepHook = func() time.Duration {
			d := 10 * time.Millisecond
			sleeps = append(sleeps, d)
			return d
		}
		return e, &sleeps
	}

	on := DefaultPacing()
	e, sleeps := newEngine(&on)
	start := time.Now()
	recs, _, err := e.fetchPages(context.Background(), "pacing-search", nil, nil, model0Cursor(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || pagesServed != 3 {
		t.Fatalf("walk shape changed: recs=%d pages=%d", len(recs), pagesServed)
	}
	if len(*sleeps) != 2 {
		t.Fatalf("want exactly pages-1=2 sleeps, got %d", len(*sleeps))
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("sleeps did not actually elapse")
	}

	pagesServed = 0
	off := DefaultPacing()
	off.Enabled = false
	e2, sleeps2 := newEngine(&off)
	if _, _, err := e2.fetchPages(context.Background(), "pacing-search", nil, nil, model0Cursor(), 10); err != nil {
		t.Fatal(err)
	}
	if len(*sleeps2) != 0 {
		t.Fatalf("disabled pacing must not sleep, got %d", len(*sleeps2))
	}
}

// TestPacingContractOverride verifies per-contract paging.page_sleep_ms:
// -1 disables pacing for that contract even when the global config is on.
func TestPacingContractOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		if cursor == "" {
			w.Write([]byte(`{"data":[{"id":"x"}],"has_more":true,"cursor":"p2"}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"x"}],"has_more":false}`))
	}))
	defer srv.Close()
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "pacing-off", Platform: "mock", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/list", Method: "GET"},
		Binding:   contracts.Binding{Items: "$.data"},
		Paging:    contracts.Paging{CursorParam: "cursor", HasMorePath: "$.has_more", NextCursorPath: "$.cursor", PageSleepMS: -1},
	})
	on := DefaultPacing()
	e := New(Context{Registry: reg, HTTP: httpclient.New(httpclient.Config{UserAgents: []string{"ua"}}), Pacing: &on})
	var slept int
	e.sleepHook = func() time.Duration { slept++; return 0 }
	if _, _, err := e.fetchPages(context.Background(), "pacing-off", nil, nil, model0Cursor(), 10); err != nil {
		t.Fatal(err)
	}
	if slept != 0 {
		t.Fatalf("page_sleep_ms=-1 must disable pacing for the contract, got %d sleeps", slept)
	}
	// pacingFor unit: positive override replaces the median
	p := pacingFor(DefaultPacing(), 500)
	if p.Median != 500*time.Millisecond || !p.Enabled {
		t.Fatalf("positive override not applied: %+v", p)
	}
}

// TestPacingFromEnvSmoke: the package-level test env disables pacing by
// default (see pacing_testenv_test.go) — restore and verify a normal value.
func TestPacingFromEnvSmoke(t *testing.T) {
	old, had := os.LookupEnv("MEDIAMON_PAGE_SLEEP_MS")
	defer func() {
		if had {
			os.Setenv("MEDIAMON_PAGE_SLEEP_MS", old)
		} else {
			os.Unsetenv("MEDIAMON_PAGE_SLEEP_MS")
		}
	}()
	os.Unsetenv("MEDIAMON_PAGE_SLEEP_MS")
	if p := PacingFromEnv(); !p.Enabled || p.Median != 1500*time.Millisecond || p.Sigma != 1.0 {
		t.Fatalf("defaults wrong: %+v", p)
	}
	os.Setenv("MEDIAMON_PAGE_SLEEP_MS", "0")
	if p := PacingFromEnv(); p.Enabled {
		t.Fatal("MEDIAMON_PAGE_SLEEP_MS=0 must disable pacing")
	}
}

func model0Cursor() model.Cursor { return model.Cursor{} }
