// browserhdr.go — browser-grade request headers + per-session HTTP clients
// (silent-scraping round 2, report item 2 / B1+B3). The engine merges, in
// increasing precedence:
//
//	platform browser defaults (Context.BrowserHeaders, built by
//	  internal/platforms/<p>.BrowserHeaders — referer, accept,
//	  accept-language, sec-fetch-*, priority, cache-control…)
//	< signer output riding headers (x-s / x-s-common)
//	< contract transport.headers (deploy-specific overrides)
//	< UA-consistent client hints (sec-ch-ua / -mobile / -platform derived
//	  from the SAME pinned UA that will be sent — never a constant)
//	< Cookie / User-Agent (per-identity)
//
// Session binding (B3): every (account|platform-default, proxy) pair gets a
// dedicated httpclient with its own cookie jar, so Set-Cookie rotations
// (msToken et al.) persist within one cookie lifetime and never leak across
// identities.
package collect

import (
	"regexp"
	"strings"
	"sync"

	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
)

// chromeVerRe extracts the Chromium major version from a UA string.
var chromeVerRe = regexp.MustCompile(`(?:Chrome|CriOS)/(\d+)`)

// deriveClientHints computes the sec-ch-ua header family that a real Chrome
// would send WITH this exact UA (brand list matches the Chromium major
// version, Edge gets the Microsoft Edge brand, mobile gets ?1, platform
// matches the OS token). Non-Chromium UAs (Firefox/Safari) send no client
// hints at all — nil return.
func deriveClientHints(ua string) map[string]string {
	m := chromeVerRe.FindStringSubmatch(ua)
	if m == nil {
		return nil
	}
	v := m[1]
	mobile := strings.Contains(ua, "Mobile") || strings.Contains(ua, "Android") || strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad")
	platform := `"Windows"`
	switch {
	case strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad"):
		platform = `"iOS"`
	case strings.Contains(ua, "Macintosh"):
		platform = `"macOS"`
	case strings.Contains(ua, "Linux") && !strings.Contains(ua, "Android"):
		platform = `"Linux"`
	case strings.Contains(ua, "Android"):
		platform = `"Android"`
	}
	mobileVal := "?0"
	if mobile {
		mobileVal = "?1"
	}
	brand := `"Chromium";v="` + v + `", "Not?A_Brand";v="24", "Google Chrome";v="` + v + `"`
	if strings.Contains(ua, "Edg/") {
		brand = `"Chromium";v="` + v + `", "Not=A?Brand";v="24", "Microsoft Edge";v="` + v + `"`
	}
	return map[string]string{
		"sec-ch-ua":          brand,
		"sec-ch-ua-mobile":   mobileVal,
		"sec-ch-ua-platform": platform,
	}
}

// browserHeadersFor returns the platform's default browser header set (nil
// for unknown platforms — engine callers may register custom platforms via
// Context.BrowserHeaders).
func (e *Engine) browserHeadersFor(platform string) map[string]string {
	if e.browserHdrs == nil {
		return nil
	}
	return e.browserHdrs[platform]
}

// sessionCache memoizes jar-backed HTTP clients per (session key, proxy).
// One cache is shared by every clone of an engine (rotation included), so a
// cookie lifetime spans account switches of OTHER ids while each id keeps
// its own jar.
type sessionCache struct {
	mu    sync.Mutex
	byKey map[string]*httpclient.Client
}

func newSessionCache() *sessionCache {
	return &sessionCache{byKey: map[string]*httpclient.Client{}}
}

// sessionKey identifies one cookie lifetime: the bound account id, or the
// platform default identity when no account is selected.
func (e *Engine) sessionKey(platform string) string {
	if e.accountID != "" {
		return "account:" + e.accountID
	}
	return "platform:" + platform
}

// sessionClient returns the jar-backed client for one session+proxy pair,
// creating it from the base/proxy client on first use.
func (e *Engine) sessionClient(base *httpclient.Client, key string) *httpclient.Client {
	if e.sess == nil {
		return base
	}
	e.sess.mu.Lock()
	defer e.sess.mu.Unlock()
	if c, ok := e.sess.byKey[key]; ok {
		return c
	}
	c := base.Session()
	e.sess.byKey[key] = c
	return c
}

// fetchClient resolves the HTTP client for one contract fetch: the
// session-bound (cookie-jar) client routed through the account's proxy when
// set, else the shared session client over the plain client.
func (e *Engine) fetchClient(platform, proxy string) *httpclient.Client {
	return e.sessionClient(e.clientFor(proxy), e.sessionKey(platform))
}

// fallbackSessionUA is the deterministic fallback when no UA pool is wired:
// the current corpus-era desktop Chrome (a real, existing UA — report B2).
const fallbackSessionUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// resolveUA returns the session-pinned User-Agent for this engine's current
// identity (report B2/B3): ONE UA per cookie lifetime — the account's pinned
// UA when the account carries one, otherwise a UA drawn once from the pool
// and cached under the session key. Switching accounts switches the session
// key, hence the UA; the UA NEVER rotates per request.
func (e *Engine) resolveUA(platform string) string {
	key := e.sessionKey(platform)
	e.uaMu.Lock()
	defer e.uaMu.Unlock()
	if ua, ok := e.uaByPlat[key]; ok {
		return ua
	}
	ua := fallbackSessionUA
	if e.uaPool != nil {
		if picked := e.uaPool.Pick(); picked != "" {
			ua = picked
		}
	}
	if e.uaByPlat == nil {
		e.uaByPlat = map[string]string{}
	}
	e.uaByPlat[key] = ua
	return ua
}
