package main

import (
	"fmt"
	"os"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/kuaishou"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/xhs"
)

// wiring.go — shared cmd-layer wiring: account-pool injection, the UA
// rotation pool for the shared HTTP client, and the collect wiring shared by the gated
// every collection/action command (collect, send, trace, live monitor).

// accountPoolFor opens the account pool and validates that account id exists
// and belongs to platform. An empty id returns (nil, nil): the caller keeps
// the platform-level defaults (backward compatible). The pool is left open on
// purpose — the collect engine reads from it for the process lifetime (the
// CLI is one-shot).
func accountPoolFor(platform, id string) (*accounts.Pool, error) {
	if id == "" {
		return nil, nil
	}
	pool, err := accounts.Open(accountsDir())
	if err != nil {
		return nil, err
	}
	a, ok := pool.Get(id)
	if !ok {
		return nil, fmt.Errorf("account %q not found in pool %s", id, accountsDir())
	}
	if a.Platform != platform {
		return nil, fmt.Errorf("account %q belongs to platform %q, not %q", id, a.Platform, platform)
	}
	return pool, nil
}

// uaPoolUserAgents loads the UA rotation pool for the shared HTTP client:
// $MEDIAMON_UA_POOL when set, otherwise the executable-relative
// data/ua-pool.json (accounts.LoadUAPoolDefault). The pool is an enhancement,
// not a gate: a missing/unreadable file returns nil and the client keeps its
// built-in rotation.
func uaPoolUserAgents() []string {
	pool, err := accounts.LoadUAPoolDefault(os.Getenv("MEDIAMON_UA_POOL"))
	if err != nil {
		return nil
	}
	// UAPool exposes rotation only; drain it until every entry was seen so the
	// HTTP client rotates over the full pool (bounded — duplicates are skipped).
	n := pool.Len()
	seen := map[string]bool{}
	out := make([]string, 0, n)
	for i := 0; i < n*8 && len(out) < n; i++ {
		if ua := pool.Next(); ua != "" && !seen[ua] {
			seen[ua] = true
			out = append(out, ua)
		}
	}
	return out
}

// sharedHTTPClient builds the HTTP client shared by the collect/send/live
// engines, with the UA pool injected when available.
func sharedHTTPClient() *httpclient.Client {
	return httpclient.New(httpclient.Config{UserAgents: uaPoolUserAgents()})
}

// browserHeaderDefaults assembles the per-platform browser-grade header sets
// (silent-scraping B1): referer/accept/accept-language/sec-fetch-*/priority
// defaults the engine merges under every request; contract transport.headers
// can still override each value.
func browserHeaderDefaults() map[string]map[string]string {
	return map[string]map[string]string{
		douyin.Platform:   douyin.BrowserHeaders(),
		kuaishou.Platform: kuaishou.BrowserHeaders(),
		xhs.Platform:      xhs.BrowserHeaders(),
	}
}
