// Account health model + probe classification (IR-MM-0001 AC-8 /
// BEH-5..7). The classification is pure and platform-agnostic; the walk
// that produces the observation lives in internal/collect (it needs the
// contract registry and the engine's account context).
package accounts

import (
	"fmt"
	"time"
)

// Health is the probe-derived account state. Empty = never probed.
type Health string

const (
	HealthHealthy  Health = "healthy"  // full walk OK (incl. pagination depth)
	HealthDegraded Health = "degraded" // partial success (e.g. page 2 transport failure)
	HealthExpired  Health = "expired"  // auth wall / risk control / 200+empty page
)

// ProbeWalk is the raw observation of one probe walk. Status 0 with
// Err != nil means a transport-level failure; Empty means the response was
// 2xx but the contract's primary binding resolved to an empty list.
type ProbeWalk struct {
	Page1Status  int
	Page1Err     error
	Page1Empty   bool
	Page1HasMore bool
	DepthChecked bool // contract paginates and a second page was requested
	Page2Status  int
	Page2Err     error
	Page2Empty   bool
}

// ClassifyHealth maps a walk observation onto the three health forms
// (BEH-5..7):
//   - 401/403                      → expired (auth/risk wall)
//   - 2xx + empty primary binding  → expired (HTTP 200 + empty body, the
//     f2 #435 half-dead-cookie pattern — page 1 may look fine, depth dies)
//   - page 1 OK, page 2 non-auth
//     transport/5xx failure        → degraded (partial success)
//   - full walk OK                 → healthy
func ClassifyHealth(w ProbeWalk) (Health, string) {
	if w.Page1Err != nil {
		if w.Page1Status == 401 || w.Page1Status == 403 {
			return HealthExpired, fmt.Sprintf("auth wall: status %d", w.Page1Status)
		}
		if w.Page1Status >= 500 || w.Page1Status == 0 {
			return HealthDegraded, fmt.Sprintf("page 1 unavailable: %v (status %d)", w.Page1Err, w.Page1Status)
		}
		return HealthDegraded, fmt.Sprintf("page 1 status %d", w.Page1Status)
	}
	if w.Page1Empty {
		return HealthExpired, "page 1 empty (200 + empty body)"
	}
	if !w.DepthChecked {
		return HealthHealthy, "single-shot ok"
	}
	if w.Page2Err != nil {
		if w.Page2Status == 401 || w.Page2Status == 403 {
			return HealthExpired, fmt.Sprintf("depth auth wall: status %d", w.Page2Status)
		}
		return HealthDegraded, fmt.Sprintf("page 2 unavailable: %v (status %d)", w.Page2Err, w.Page2Status)
	}
	if w.Page2Empty {
		return HealthExpired, "page 2 empty (200 + empty body at depth)"
	}
	return HealthHealthy, "pagination depth ok"
}

// SetHealth persists one account's probe outcome (health + timestamp +
// detail). The snapshot row the pool appends carries all three fields, so
// health survives restarts (W4-C1 AC-1).
func (p *Pool) SetHealth(id string, h Health, detail string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadAll()
	a, ok := p.cache[id]
	if !ok {
		return fmt.Errorf("accounts: %q not found", id)
	}
	a.Health = h
	a.HealthCheckedAt = time.Now().Unix()
	a.HealthDetail = detail
	a.UpdatedAt = a.HealthCheckedAt
	p.cache[id] = a
	return p.persist()
}
