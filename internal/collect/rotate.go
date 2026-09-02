// Auto account rotation (IR-MM-0001 AC-9): AccountID "auto" picks accounts
// by health (healthy → degraded → unprobed; expired/banned excluded),
// rotates on auth walls / empty pages with the cursor preserved, retries
// bounded at 2 switches, and bans an account after three consecutive
// failures. Rotation and ban events count into /metrics observability.
package collect

import (
	"errors"
	"fmt"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
)

// autoAccountID selects accounts automatically by pool health.
const autoAccountID = "auto"

// maxRotations bounds account switches per walk (IR budget: retry ≤2).
const maxRotations = 2

// errorsIs is errors.Is (indirection keeps the hook in predicate.go lean).
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// isAutoAccount reports whether this engine runs in auto-selection mode —
// either it is the unbound "auto" engine or a rotation clone (autoBase).
func (e *Engine) isAutoAccount() bool {
	return e.accountID == autoAccountID || e.autoBase != nil
}

// currentAccount returns the bound account id ("" = platform defaults).
func (e *Engine) currentAccount() string { return e.accountID }

// bindInitial resolves the first account for auto mode (health-ranked).
// Fails closed when the pool is missing or empty.
func (e *Engine) bindInitial(name string) (*Engine, error) {
	if e.accounts == nil {
		return nil, fmt.Errorf("collect %s: auto account mode requires a configured pool", name)
	}
	a, ok := e.accounts.PickFor(e.platformFor(name), nil)
	if !ok {
		return nil, fmt.Errorf("collect %s: no usable account in pool (auto mode)", name)
	}
	ne := e.forAccount(a.ID)
	ne.autoBase = e // keep the "auto" marker so rotation stays armed
	return ne, nil
}

// rotateOn handles one rotation-eligible failure: the failing account gets
// a consecutive-failure mark (banning at the threshold), the next usable
// account is picked, and a fetch-engine clone bound to it is returned. The
// caller retries the same page with it.
func (e *Engine) rotateOn(name string, cause error, tried map[string]bool, rotations *int) (*Engine, error) {
	if e.accounts == nil {
		return nil, cause
	}
	if id := e.currentAccount(); id != "" && id != autoAccountID {
		banned, err := e.accounts.MarkFailure(id)
		if err == nil && e.obs != nil {
			e.obs.Inc("accounts.rotation.total", 1)
			if banned {
				e.obs.Inc("accounts.banned.total", 1)
			}
		}
		tried[id] = true
	}
	if *rotations >= maxRotations {
		return nil, fmt.Errorf("collect %s: auto rotation exhausted after %d switches: %w", name, *rotations, cause)
	}
	next, ok := e.accounts.PickFor(e.platformFor(name), tried)
	if !ok {
		return nil, fmt.Errorf("collect %s: no usable account left in pool (auto mode): %w", name, cause)
	}
	tried[next.ID] = true
	*rotations++
	ne := e.forAccount(next.ID)
	ne.autoBase = e.autoBase
	return ne, nil
}

// platformFor resolves a contract's platform by name.
func (e *Engine) platformFor(name string) string {
	if c, ok := e.reg.Get(name); ok {
		return c.Platform
	}
	return ""
}

// forAccount clones the engine with a specific account selected. The clone
// keeps rotation armed in auto mode (autoBase marker) and inherits the base
// engine's pacing config + test sleep hook so think-time behavior survives
// account switches.
func (e *Engine) forAccount(accountID string) *Engine {
	base := e.autoBase
	ne := New(Context{
		Registry:  e.reg,
		HTTP:      e.hc,
		Obs:       e.obs,
		Signers:   e.signers,
		Cookies:   e.cookies,
		Names:     e.names,
		Accounts:  e.accounts,
		AccountID: accountID,
		Pacing:    &e.pacing,
	})
	ne.autoBase = base
	ne.sleepHook = e.sleepHook
	// Share the browser header table, the cookie-session cache and the UA
	// pin table with the base engine: rotation clones keep the same header
	// posture, each account id keeps its own jar AND its own pinned UA
	// (B1/B2/B3 — a session's UA changes only when the account changes).
	ne.browserHdrs = e.browserHdrs
	ne.sess = e.sess
	ne.uaByPlat = e.uaByPlat
	ne.uaPool = e.uaPool
	return ne
}

var _ = accounts.StatusBanned
