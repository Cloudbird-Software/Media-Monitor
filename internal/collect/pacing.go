// pacing.go — human-shaped inter-page pacing (silent-scraping round 2,
// test-report-round1 item 1 / A1+A2). Real users never page at a constant
// server-echo cadence: the recorded corpus shows a heavy-tailed distribution
// (dy same-endpoint paging mean≈207ms max 2.2s; xhs max 6.1s; ks mean≈1.9s
// max 28s). The engine therefore sleeps between consecutive pages with a
// log-normal "think time" (median μ, log-space σ), clamped to [Min,Max].
//
// Configuration (contract or global, default ON):
//   - global env MEDIAMON_PAGE_SLEEP_MS    median think time (default 1500;
//     0 disables pacing entirely)
//   - global env MEDIAMON_PAGE_SLEEP_SIGMA log-space sigma (default 1.0)
//   - global env MEDIAMON_EMERGENCY        any non-empty value disables
//     pacing (emergency/incident mode: page as fast as the server allows)
//   - per-contract paging.page_sleep_ms    overrides the median for that
//     contract; -1 disables pacing for that contract only
package collect

import (
	"context"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

// PacingConfig tunes the inter-page think time.
type PacingConfig struct {
	// Enabled turns pacing on/off globally (default true).
	Enabled bool
	// Median is the log-normal median (p50) of the think time.
	Median time.Duration
	// Sigma is the log-space standard deviation (heavy tail driver:
	// 1.0 gives p90≈3.6×median, p99≈10×median).
	Sigma float64
	// Min / Max clamp the sampled delay (default 250ms / 30s; the corpus
	// ceiling is ks max 28s).
	Min time.Duration
	Max time.Duration
}

// DefaultPacing returns the production defaults: median 1.5s, sigma 1.0,
// clamped to [250ms, 30s]. p50=1.5s p90≈5.4s p99≈15s — matching the shape of
// the human baseline in test_report_round1.md §4-A1 (report recommendation:
// p50≈1.5s, tail 8s+).
func DefaultPacing() PacingConfig {
	return PacingConfig{
		Enabled: true,
		Median:  1500 * time.Millisecond,
		Sigma:   1.0,
		Min:     250 * time.Millisecond,
		Max:     30 * time.Second,
	}
}

// envMS reads a plain integer-milliseconds env var.
func envMS(name string) (time.Duration, bool) {
	n, ok := envInt(name)
	if !ok || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Millisecond, true
}

// envInt reads a plain integer env var (shared by pacing/count/pages knobs).
func envInt(name string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// envFloat reads a float env var.
func envFloat(name string) (float64, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return f, true
}

// PacingFromEnv resolves the global pacing config: defaults, then
// MEDIAMON_PAGE_SLEEP_MS / MEDIAMON_PAGE_SLEEP_SIGMA overrides, then the
// MEDIAMON_EMERGENCY kill-switch (any non-empty non-"0" value disables
// pacing — the emergency-mode escape hatch required by the report).
func PacingFromEnv() PacingConfig {
	p := DefaultPacing()
	if d, ok := envMS("MEDIAMON_PAGE_SLEEP_MS"); ok {
		p.Median = d
		if d == 0 {
			p.Enabled = false
		}
	}
	if s, ok := envFloat("MEDIAMON_PAGE_SLEEP_SIGMA"); ok {
		p.Sigma = s
	}
	if v := strings.TrimSpace(os.Getenv("MEDIAMON_EMERGENCY")); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		p.Enabled = false
	}
	return p
}

// pacingFor resolves the effective pacing for one contract: the global config
// narrowed by the contract's paging.page_sleep_ms override (positive value
// replaces the median; -1 disables pacing for this contract).
func pacingFor(global PacingConfig, contractPageSleepMS int) PacingConfig {
	p := global
	switch {
	case contractPageSleepMS == 0: // unset → inherit global
	case contractPageSleepMS < 0: // explicit off for this contract
		p.Enabled = false
	default:
		p.Median = time.Duration(contractPageSleepMS) * time.Millisecond
	}
	return p
}

// lognormalSleep samples one think time: exp(ln(median) + σ·z) clamped to
// [Min,Max]. A disabled or degenerate config returns 0.
func (p PacingConfig) lognormalSleep(rnd *rand.Rand) time.Duration {
	if !p.Enabled || p.Median <= 0 || p.Sigma <= 0 || p.Max <= 0 {
		return 0
	}
	z := rnd.NormFloat64()
	d := time.Duration(math.Exp(math.Log(float64(p.Median)) + p.Sigma*z))
	if d < p.Min {
		d = p.Min
	}
	if d > p.Max {
		d = p.Max
	}
	return d
}

// pageThink parks the walk for one sampled think time before the next page
// fetch. Cancellation-aware: ctx end returns immediately. The sleepHook seam
// (tests only) replaces the random sample with a deterministic value.
func (e *Engine) pageThink(ctx context.Context, p PacingConfig) {
	if !p.Enabled {
		return
	}
	var d time.Duration
	if e.sleepHook != nil {
		d = e.sleepHook()
	} else {
		d = p.lognormalSleep(e.pacingRNG())
		if e.obs != nil && d > 0 {
			e.obs.Inc("collect.page_sleep_total_ms", int64(d/time.Millisecond))
		}
	}
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// pacingRNG returns the engine-local pacing RNG under its mutex.
func (e *Engine) pacingRNG() *rand.Rand {
	e.pacingMu.Lock()
	defer e.pacingMu.Unlock()
	return e.pacingRand
}

// pacingSource seeds a fresh engine-local RNG (kept separate from the global
// rand source so tests can drive deterministic samples).
func newPacingRand() *rand.Rand {
	return rand.New(rand.NewSource(time.Now().UnixNano()))
}
