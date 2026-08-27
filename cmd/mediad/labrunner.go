// lab runner loop (IR-MM-0001 AC-17 前半 / BEH-14..16): the self-optimizing
// lab's local scheduling carrier. Cadence (env-tunable): offline-canary
// refresh every MEDIAMON_LAB_CANARY_INTERVAL (default 6h) feeding the
// contract-health timeline, account probe every MEDIAMON_LAB_PROBE_INTERVAL
// (default 2h) keeping pool health fresh for auto rotation. The GitHub-side
// schedule (workflow runs, drift-issue claim, fix-PR re-run backfill) is the
// owner-side wiring documented in docs/LAB.md — the App cannot touch
// .github/workflows/** (org NONGOAL).
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
)

// labLoop runs the two lab cadences until the daemon context ends.
func (d *daemon) startLabLoop(ctx context.Context) {
	canaryIv := labEnvDuration("MEDIAMON_LAB_CANARY_INTERVAL", 6*time.Hour)
	probeIv := labEnvDuration("MEDIAMON_LAB_PROBE_INTERVAL", 2*time.Hour)
	if canaryIv <= 0 || probeIv <= 0 {
		log.Printf("lab: invalid intervals (canary=%v probe=%v) — loop disabled (fail-closed)", canaryIv, probeIv)
		return
	}
	go d.labTicker(ctx, canaryIv, d.labCanaryCycle)
	go d.labTicker(ctx, probeIv, d.labProbeCycle)
}

// labEnvDuration reads an env override with a lab default (invalid → -1
// so the loop disables itself fail-closed).
func labEnvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	p, err := time.ParseDuration(v)
	if err != nil || p <= 0 {
		return -1
	}
	return p
}

// labTicker runs one cycle fn on the interval.
func (d *daemon) labTicker(ctx context.Context, iv time.Duration, fn func()) {
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// labCanaryCycle refreshes the offline canary snapshot and records the day
// health (the timeline the dashboard renders).
func (d *daemon) labCanaryCycle() {
	d.wireAdapt(d.dataDir)
	green := d.canary != nil && d.canary.Err == "" && d.canary.Healthy
	d.recordDayHealth(green, "")
	if green {
		log.Printf("lab: canary cycle green (%d cases)", d.canary.Cases)
	} else if d.canary != nil {
		log.Printf("lab: canary cycle RED: %s", d.canary.Summary)
	}
}

// labProbeCycle probes every pool account (health feeds auto rotation and
// the water-level alarm). The probe's default contract per platform comes
// from collect.DefaultProbeContract; platforms without one are skipped
// explicitly (logged, never silent).
func (d *daemon) labProbeCycle() {
	if d.accounts == nil || d.engine == nil {
		return
	}
	probed, failed, skipped := 0, 0, 0
	for _, a := range d.accounts.List() {
		name, params, err := collect.DefaultProbeContract(a.Platform)
		if err != nil {
			skipped++
			continue
		}
		// xhs probes need the target user_id: read from tags when present
		if params != nil {
			if uid := accountTag(a, "probe_user_id"); uid != "" {
				params["user_id"] = uid
			}
		}
		eng := d.engineFor(a.ID)
		if _, err := eng.ProbeAndStore(context.Background(), d.accounts, a.ID, name, params); err != nil {
			failed++
			log.Printf("lab: probe %s: %v", a.ID, err)
			continue
		}
		probed++
	}
	log.Printf("lab: probe cycle done (ok=%d failed=%d skipped=%d)", probed, failed, skipped)
}

// accountTag reads one "key:value" tag off an account row.
func accountTag(a accounts.Account, key string) string {
	prefix := key + ":"
	for _, t := range a.Tags {
		if len(t) > len(prefix) && t[:len(prefix)] == prefix {
			return t[len(prefix):]
		}
	}
	return ""
}
