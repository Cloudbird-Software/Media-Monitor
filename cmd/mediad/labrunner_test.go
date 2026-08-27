package main

import (
	"context"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
)

// TestLabEnvDuration: env override + default + invalid fail-closed (-1).
func TestLabEnvDuration(t *testing.T) {
	t.Setenv("MEDIAMON_LAB_CANARY_INTERVAL", "")
	if got := labEnvDuration("MEDIAMON_LAB_CANARY_INTERVAL", 6*time.Hour); got != 6*time.Hour {
		t.Fatalf("default = %v", got)
	}
	t.Setenv("MEDIAMON_LAB_CANARY_INTERVAL", "90m")
	if got := labEnvDuration("MEDIAMON_LAB_CANARY_INTERVAL", 6*time.Hour); got != 90*time.Minute {
		t.Fatalf("override = %v", got)
	}
	t.Setenv("MEDIAMON_LAB_CANARY_INTERVAL", "bogus")
	if got := labEnvDuration("MEDIAMON_LAB_CANARY_INTERVAL", 6*time.Hour); got != -1 {
		t.Fatalf("invalid = %v, want -1 (fail-closed disable)", got)
	}
}

// TestAccountTag: probe_user_id rides the account tags.
func TestAccountTag(t *testing.T) {
	a := accounts.Account{ID: "a1", Platform: "xhs", Tags: []string{"probe_user_id:u-123", "other:x"}}
	if got := accountTag(a, "probe_user_id"); got != "u-123" {
		t.Fatalf("tag = %q", got)
	}
	if got := accountTag(a, "missing"); got != "" {
		t.Fatalf("missing tag = %q", got)
	}
}

// TestLabTickerStopsOnCancel: the loop respects the daemon lifetime.
func TestLabTickerStopsOnCancel(t *testing.T) {
	d := &daemon{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.labTicker(ctx, time.Hour, func() {}); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("labTicker did not stop on context cancel")
	}
}
