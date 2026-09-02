// Account health probe walk (IR-MM-0001 AC-8 / BEH-5..7). The walk drives
// a platform's cheapest declared contract through the engine's normal
// request machinery so the probe observes exactly what collection would
// see — the account's own cookie/proxy/UA (accountContext), the contract's
// signature/cookie fail-closed gates, and (for paginated contracts) the
// page-2 depth anomaly that marks a half-dead cookie (f2 #435).
package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// ProbeOutcome is the persisted result of one account probe.
type ProbeOutcome struct {
	Health    accounts.Health `json:"health"`
	Detail    string          `json:"detail"`
	CheckedAt int64           `json:"checked_at"`
}

// defaultProbeContract names each platform's cheapest declared contract for
// probing. Kuaishou has no cheap single-shot surface declared — probing it
// fails closed with an explicit error until one lands.
var defaultProbeContract = map[string]string{
	"douyin": "douyin-im-unread",
	"xhs":    "xhs-user-notes",
}

// DefaultProbeContract returns the probe contract for a platform. Params
// carries the contract's required placeholders (e.g. xhs user-notes needs
// sec_uid as the probe target).
func DefaultProbeContract(platform string) (string, map[string]string, error) {
	name, ok := defaultProbeContract[platform]
	if !ok {
		return "", nil, fmt.Errorf("probe: no probe contract declared for platform %q", platform)
	}
	params := map[string]string{}
	if platform == "xhs" {
		// xhs-user-notes locates the target by user_id (the probe walks the
		// account's own note list — depth check applies natively).
		params["user_id"] = ""
	}
	return name, params, nil
}

// probePage is the raw outcome of one request.
type probePage struct {
	status int
	err    error
	empty  bool
}

// probeFetch executes one contract request through the engine's full
// machinery (signing, cookies, proxy, account context) and reports the raw
// status plus whether the primary binding resolved to an empty list.
func (e *Engine) probeFetch(ctx context.Context, c *contracts.Contract, pathParams, query map[string]string) probePage {
	full, headers, body, err := e.buildURL(ctx, c, pathParams, query)
	if err != nil {
		// fail-closed gate (missing cookie/signature/placeholder): the
		// account cannot exercise this contract at all — expired-level.
		return probePage{status: http.StatusForbidden, err: err}
	}
	_, proxy, _, _ := e.accountContext(c.Platform)
	hc := e.fetchClient(c.Platform, proxy)
	status, resp, err := hc.WithContract(c.Name).Do(ctx, c.Transport.Method, full, headers, body)
	if err != nil {
		return probePage{err: err}
	}
	if status < 200 || status >= 300 {
		return probePage{status: status, err: fmt.Errorf("status %d", status)}
	}
	var doc map[string]any
	if err := json.Unmarshal(resp, &doc); err != nil || doc == nil {
		return probePage{status: status, err: fmt.Errorf("response not a JSON object")}
	}
	return probePage{status: status, empty: bindingEmpty(c, doc)}
}

// ProbeAccount walks the probe contract for one account and classifies the
// observation (accounts.ClassifyHealth). The engine must be built with the
// target AccountID so every request rides the account's own
// cookie/proxy/UA (W4-C1 AC-5).
func (e *Engine) ProbeAccount(ctx context.Context, platform, contract string, params map[string]string) (ProbeOutcome, error) {
	if contract == "" {
		var err error
		var defaults map[string]string
		contract, defaults, err = DefaultProbeContract(platform)
		if err != nil {
			return ProbeOutcome{}, err
		}
		// Caller-supplied params must survive default-contract resolution:
		// overlay them on the platform defaults (B1 — the previous
		// reassignment dropped them, so the documented --param usage could
		// never reach the placeholder check).
		if len(params) > 0 {
			merged := make(map[string]string, len(defaults)+len(params))
			for k, v := range defaults {
				merged[k] = v
			}
			for k, v := range params {
				merged[k] = v
			}
			params = merged
		}
	}
	c, ok := e.reg.Get(contract)
	if !ok {
		return ProbeOutcome{}, fmt.Errorf("probe: contract %q not registered", contract)
	}
	for _, p := range c.Transport.Placeholders {
		if params[p] == "" {
			return ProbeOutcome{}, fmt.Errorf("probe: contract %q requires param %q (pass --param %s=<...>)", contract, p, p)
		}
	}
	walk := accounts.ProbeWalk{}
	p1 := e.probeFetch(ctx, c, params, nil)
	walk.Page1Status, walk.Page1Err, walk.Page1Empty = p1.status, p1.err, p1.empty
	if p1.err == nil && !p1.empty {
		if c.Paging.NextCursorPath != "" {
			walk.DepthChecked = true
			// depth check: re-run the fetch with the paging cursor forced to
			// a second-page position — a half-dead cookie answers 200 + empty
			// here while page 1 still looked alive (f2 #435).
			p2 := e.probeFetch(ctx, c, params, map[string]string{c.Paging.CursorParam: probeDepthCursor})
			walk.Page2Status, walk.Page2Err, walk.Page2Empty = p2.status, p2.err, p2.empty
		}
	}
	h, detail := accounts.ClassifyHealth(walk)
	return ProbeOutcome{Health: h, Detail: detail, CheckedAt: time.Now().Unix()}, nil
}

// probeDepthCursor is the page-2 position marker: most cursor schemes treat
// a non-zero numeric cursor as "resume deep"; for offset-style params this
// skips the first page; for max_cursor-style params the platform returns
// the tail window. It is a depth probe, not a data fetch.
const probeDepthCursor = "20"

// bindingEmpty reports whether the contract's primary list binding is
// missing or empty in a 2xx document (the 200+empty-body half-dead-cookie
// signature).
func bindingEmpty(c *contracts.Contract, doc map[string]any) bool {
	_, raw := mainBindingRaw(c)
	if raw == "" {
		return false // fields-only contract: emptiness is not expressible
	}
	p, err := contracts.ParsePath(raw)
	if err != nil {
		return false
	}
	vs := p.Select(doc)
	if len(vs) == 0 {
		return true
	}
	list, ok := vs[0].([]any)
	return ok && len(list) == 0
}

// errNoPool is the explicit no-pool error (fail-closed, never silent).
var errNoPool = errors.New("probe: account pool not configured")

// ProbeAndStore runs ProbeAccount and persists the outcome on the pool.
func (e *Engine) ProbeAndStore(ctx context.Context, pool *accounts.Pool, accountID, contract string, params map[string]string) (ProbeOutcome, error) {
	if pool == nil {
		return ProbeOutcome{}, errNoPool
	}
	a, ok := pool.Get(accountID)
	if !ok {
		return ProbeOutcome{}, fmt.Errorf("probe: account %q not found", accountID)
	}
	pe := e.forAccount(accountID)
	out, err := pe.ProbeAccount(ctx, a.Platform, contract, params)
	if err != nil {
		return out, err
	}
	if serr := pool.SetHealth(accountID, out.Health, out.Detail); serr != nil {
		return out, fmt.Errorf("probe: persist health: %w", serr)
	}
	return out, nil
}
