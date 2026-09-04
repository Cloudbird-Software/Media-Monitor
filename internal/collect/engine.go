// Package collect — contract-driven collection engine. See binder.go for the
// raw-JSON → model mapping and engine.go for the fetch/pagination machinery.
package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

// Context wires the engine's dependencies. Registry+HTTP are required for
// real collection; the rest are optional per-platform extensions.
type Context struct {
	Registry *contracts.Registry
	HTTP     *httpclient.Client
	Obs      *obs.CounterMap
	// Signers keyed by platform: the signer's returned kv map is merged into
	// the outgoing query on every request for that platform.
	Signers map[string]httpclient.Signer
	// Cookies keyed by platform; the value is a "k1=v1; k2=v2" header
	// fragment merged into every request for that platform.
	Cookies map[string]string
	// Names maps platform → collect category (search/comments/replies/user/
	// group) → contract name; "" means the category is not declared. When
	// absent, the engine falls back to the "<platform>-<category>" naming
	// convention. Built by internal/platforms.
	Names map[string]map[string]string
	// Accounts is the optional account pool. When set together with AccountID,
	// requests are routed through the account's cookie/proxy/UA.
	Accounts *accounts.Pool
	// AccountID selects the account to act as; "" means platform defaults.
	AccountID string
	// Pacing overrides the inter-page think-time config (nil = env/defaults,
	// see pacing.go). The zero Context keeps pacing ON with defaults.
	Pacing *PacingConfig
	// BrowserHeaders carries per-platform browser-grade default header sets
	// (see internal/platforms/<platform>.BrowserHeaders). Merged UNDER the
	// contract transport.headers; empty map = engine sends no browser
	// defaults (legacy behavior).
	BrowserHeaders map[string]map[string]string
	// UAPool supplies the session-pinned User-Agents (one UA per cookie
	// lifetime; nil = a deterministic real-Chrome fallback). Wire with
	// accounts.LoadUAPoolDefault (MEDIAMON_UA_POOL override).
	UAPool *accounts.UAPool
}

// Engine executes contracts. Safe for concurrent use once built.
type Engine struct {
	reg         *contracts.Registry
	hc          *httpclient.Client
	obs         *obs.CounterMap
	signers     map[string]httpclient.Signer
	cookies     map[string]string
	names       map[string]map[string]string
	accounts    *accounts.Pool
	accountID   string
	autoBase    *Engine // non-nil while rotating in auto mode
	proxyMu     sync.Mutex
	proxyCache  map[string]*httpclient.Client // proxy url -> dedicated client
	pacing      PacingConfig                  // inter-page think time (pacing.go)
	pacingMu    sync.Mutex
	pacingRand  *rand.Rand
	sleepHook   func() time.Duration         // test seam: replaces the random sample
	browserHdrs map[string]map[string]string // platform -> browser default headers
	sess        *sessionCache                // cookie-jar clients per identity (shared across clones)
	uaPool      *accounts.UAPool             // session-pinned UA source (browserhdr.go)
	uaMu        sync.Mutex
	uaByPlat    map[string]string // sessionKey -> pinned session UA (binding: 1 UA per cookie lifetime)
}

// New builds an Engine from its wiring context.
func New(ctx Context) *Engine {
	pacing := PacingFromEnv()
	if ctx.Pacing != nil {
		pacing = *ctx.Pacing
	}
	e := &Engine{
		reg:         ctx.Registry,
		obs:         ctx.Obs,
		signers:     ctx.Signers,
		cookies:     ctx.Cookies,
		names:       ctx.Names,
		accounts:    ctx.Accounts,
		accountID:   ctx.AccountID,
		proxyCache:  map[string]*httpclient.Client{},
		pacing:      pacing,
		pacingRand:  newPacingRand(),
		browserHdrs: ctx.BrowserHeaders,
		sess:        newSessionCache(),
		uaPool:      ctx.UAPool,
		uaByPlat:    map[string]string{},
	}
	if ctx.HTTP != nil {
		e.hc = ctx.HTTP
	} else {
		e.hc = httpclient.New(httpclient.Config{})
	}
	return e
}

// accountContext resolves the effective cookie header, proxy and UA for a
// platform/request. Without an account (or an unknown id) it returns the
// platform-level defaults and ok=false, so callers fall back transparently.
func (e *Engine) accountContext(platform string) (cookie, proxy, ua string, ok bool) {
	if e.accounts == nil || e.accountID == "" {
		return "", "", "", false
	}
	a, found := e.accounts.Get(e.accountID)
	if !found || a.Platform != platform {
		return "", "", "", false
	}
	return a.CookieHeader(), a.Proxy, a.UA, true
}

// clientFor returns the HTTP client to use for a request carrying the given
// proxy. A non-empty proxy gets a dedicated client with a proxy transport,
// cached for reuse; an empty proxy returns the shared client.
func (e *Engine) clientFor(proxy string) *httpclient.Client {
	if proxy == "" {
		return e.hc
	}
	e.proxyMu.Lock()
	defer e.proxyMu.Unlock()
	if c, ok := e.proxyCache[proxy]; ok {
		return c
	}
	c := httpclient.New(httpclient.Config{Proxy: proxy})
	e.proxyCache[proxy] = c
	return c
}

// categorySuffix maps collect category names to the conventional
// "<platform>-<suffix>" contract naming fallback. Categories whose contract
// names differ per platform (suggest: douyin-suggest-words vs
// xhs-search-recommend) are wired explicitly through the platform Names
// maps and deliberately carry no fallback suffix.
var categorySuffix = map[string]string{
	"search":          "search",
	"comments":        "comments",
	"replies":         "replies",
	"user":            "user",
	"group":           "group-members",
	"send_message":    "send-message",
	"video_download":  "video-download",
	"collects":        "collects",
	"collects_videos": "collects-videos",
	"im_unread":       "im-unread",
	"user_posts":      "user-posts",
	"profile":         "profile",
	"user_search":     "user-search",
	"related":         "related",
}

// resolveName finds the contract name for a platform collect category,
// preferring the configured Names map and falling back to the naming
// convention.
func (e *Engine) resolveName(platform, category string) (string, error) {
	if e.names != nil {
		if m, ok := e.names[platform]; ok {
			if name, declared := m[category]; declared {
				if name == "" {
					if category == "replies" {
						return "", errors.New("replies contract not declared")
					}
					return "", fmt.Errorf("collect: %s contract not declared for platform %q", category, platform)
				}
				return name, nil
			}
		}
	}
	name := platform + "-" + categorySuffix[category]
	if _, ok := e.reg.Get(name); ok {
		return name, nil
	}
	if category == "replies" {
		return "", errors.New("replies contract not declared")
	}
	return "", fmt.Errorf("collect: %s contract not declared for platform %q", category, platform)
}

// maxPages guards against cursors that never advance.
const maxPages = 100

// placeholderRe matches "{name}" tokens in contract transport paths.
var placeholderRe = regexp.MustCompile(`\{([^{}]+)\}`)

// fillPath substitutes "{name}" tokens from pathParams and returns the
// remaining (unused) params for query/body routing. A token without a value
// is an error: contracts fail closed on missing path params.
func fillPath(path string, pathParams map[string]string) (string, map[string]string, error) {
	leftover := make(map[string]string, len(pathParams))
	for k, v := range pathParams {
		leftover[k] = v
	}
	var missing []string
	out := placeholderRe.ReplaceAllStringFunc(path, func(tok string) string {
		key := tok[1 : len(tok)-1]
		v, ok := leftover[key]
		if !ok || v == "" {
			missing = append(missing, key)
			return tok
		}
		delete(leftover, key)
		return url.PathEscape(v)
	})
	if len(missing) > 0 {
		return "", nil, fmt.Errorf("missing path param(s) %s for path %q", strings.Join(missing, ", "), path)
	}
	return out, leftover, nil
}

// buildURL assembles the final request: path substitution, static+caller
// query merge, signer decoration, signature.Required fail-closed check,
// cookie/static headers, and the POST JSON body.
func (e *Engine) buildURL(ctx context.Context, c *contracts.Contract, pathParams, query map[string]string) (string, map[string]string, []byte, error) {
	for _, p := range c.Transport.Placeholders {
		if pathParams[p] == "" {
			return "", nil, nil, fmt.Errorf("collect %s: required placeholder %q not provided", c.Name, p)
		}
	}
	path, leftover, err := fillPath(c.Transport.Path, pathParams)
	if err != nil {
		return "", nil, nil, fmt.Errorf("collect %s: build url: %w", c.Name, err)
	}
	// Merged params: transport query < leftover path params < caller query.
	params := map[string]string{}
	for k, v := range c.Transport.Query {
		params[k] = v
	}
	for k, v := range leftover {
		params[k] = v
	}
	for k, v := range query {
		params[k] = v
	}
	var body []byte
	if c.Transport.Method == http.MethodPost {
		m := map[string]any{}
		for k, v := range c.Transport.Body {
			m[k] = v
		}
		for k, v := range leftover {
			m[k] = v
		}
		for k, v := range query {
			m[k] = v
		}
		if body, err = json.Marshal(m); err != nil {
			return "", nil, nil, fmt.Errorf("collect %s: marshal body: %w", c.Name, err)
		}
	}
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	signer := e.signers[c.Platform]
	sigHeaders := map[string]string{}
	inHeader := map[string]bool{}
	for _, h := range c.Signature.Headers {
		inHeader[h] = true
	}
	if signer != nil {
		pre := c.Transport.BaseURL + path
		if len(q) > 0 {
			pre += "?" + q.Encode()
		}
		sig, serr := signer.Sign(ctx, c.Name, pre, params)
		if serr != nil {
			return "", nil, nil, fmt.Errorf("collect %s: sign: %w", c.Name, serr)
		}
		for k, v := range sig {
			// IFACE-7: values the contract routes to headers ride the
			// request headers (e.g. xhs x-s / x-s-common); the rest land
			// in the query exactly as before.
			if inHeader[k] {
				sigHeaders[k] = v
			} else {
				q.Set(k, v)
			}
		}
	}
	// Fail-closed: everything the contract marks as a required signature
	// must be present in the final URL query or — when the contract routes
	// it to headers (signature.headers) — in the signer output that will
	// ride the request headers.
	for _, rp := range c.Signature.Required {
		if q.Get(rp) != "" || sigHeaders[rp] != "" {
			continue
		}
		if inHeader[rp] {
			return "", nil, nil, fmt.Errorf("collect %s: signature required header %q missing/empty (signer output)", c.Name, rp)
		}
		return "", nil, nil, fmt.Errorf("collect %s: signature required param %q missing/empty in final URL", c.Name, rp)
	}
	full := c.Transport.BaseURL + path
	if len(q) > 0 {
		full += "?" + q.Encode()
	}
	headers := map[string]string{}
	// Precedence (low → high): platform browser defaults < signer output
	// riding headers < contract static headers < UA-consistent client hints
	// < per-identity Cookie/User-Agent set below.
	for k, v := range e.browserHeadersFor(c.Platform) {
		headers[k] = v
	}
	for k, v := range sigHeaders {
		headers[k] = v
	}
	for k, v := range c.Transport.Headers {
		headers[k] = v
	}
	if len(body) > 0 {
		headers["Content-Type"] = "application/json"
	}
	// Cookie priority: account cookie (per-identity) overrides the
	// platform-level default. The fail-closed required-cookie check below
	// runs against whichever cookie is set here.
	ck, _, ua, _ := e.accountContext(c.Platform)
	if ck == "" {
		if def, ok := e.cookies[c.Platform]; ok && def != "" {
			ck = def
		}
	}
	if ck != "" {
		headers["Cookie"] = ck
	}
	// UA priority: the account's pinned UA overrides the engine's
	// session-pinned UA (which itself overrides the client's rotating pool).
	if ua == "" {
		ua = e.resolveUA(c.Platform)
	}
	if ua != "" {
		// Client hints must match the UA being sent (sec-ch-ua family
		// derived from the same string — report B1/B2).
		for k, v := range deriveClientHints(ua) {
			headers[k] = v
		}
		headers["User-Agent"] = ua
	}
	if len(c.Cookie.Required) > 0 {
		have := strings.Split(headers["Cookie"], ";")
		names := map[string]bool{}
		for _, part := range have {
			if i := strings.IndexByte(part, '='); i > 0 {
				names[strings.TrimSpace(part[:i])] = true
			}
		}
		var missing []string
		for _, rn := range c.Cookie.Required {
			if !names[rn] {
				missing = append(missing, rn)
			}
		}
		if len(missing) > 0 {
			return "", nil, nil, fmt.Errorf("collect %s: required cookies missing: %s (see contract cookie.required)", c.Name, strings.Join(missing, ", "))
		}
	}
	return full, headers, body, nil
}

func (e *Engine) fetchErr() {
	if e.obs != nil {
		e.obs.Inc("collect.error", 1)
	}
}

// Fetch executes one contract operation and returns the parsed response
// document. The response is validated fail-closed: the contract's primary
// binding (items/comments/users/members) must be a present, non-empty list,
// and every signature.Required parameter must appear in the final URL.
func (e *Engine) Fetch(ctx context.Context, name string, pathParams, query map[string]string) (map[string]any, error) {
	c, ok := e.reg.Get(name)
	if !ok {
		return nil, fmt.Errorf("collect: contract %q not registered", name)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if e.obs != nil {
		e.obs.Inc("collect.fetch", 1)
	}
	full, headers, body, err := e.buildURL(ctx, c, pathParams, query)
	if err != nil {
		e.fetchErr()
		return nil, err
	}
	// Route through the account's proxy client when an account is selected;
	// every fetch rides the session-bound (cookie-jar) client so Set-Cookie
	// rotations persist within one identity (report B1/B3).
	_, proxy, _, _ := e.accountContext(c.Platform)
	hc := e.fetchClient(c.Platform, proxy)
	status, resp, err := hc.WithContract(name).Do(ctx, c.Transport.Method, full, headers, body)
	if err != nil {
		e.fetchErr()
		return nil, err
	}
	if status < 200 || status >= 300 {
		e.fetchErr()
		snippet := strings.TrimSpace(string(resp))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		err := fmt.Errorf("collect %s: %s status %d: %s", name, c.Transport.Method, status, snippet)
		if status == 401 || status == 403 {
			err = fmt.Errorf("%w%w", ErrAuthWall, err) // rotation-eligible
		}
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(resp, &doc); err != nil {
		e.fetchErr()
		return nil, fmt.Errorf("collect %s: response is not valid JSON: %w", name, err)
	}
	if doc == nil {
		e.fetchErr()
		return nil, fmt.Errorf("collect %s: empty response body", name)
	}
	if err := checkBindings(c, doc); err != nil {
		e.fetchErr()
		return nil, fmt.Errorf("collect %s: %w%w", name, ErrEmptyPage, err) // rotation-eligible
	}
	return doc, nil
}

// ErrAuthWall marks 401/403 responses (expired cookie / risk control) —
// rotation-eligible under auto account mode.
var ErrAuthWall = errors.New("auth wall: ")

// ErrEmptyPage marks a 2xx response whose primary binding is absent — the
// half-dead-cookie shape; rotation-eligible under auto account mode.
var ErrEmptyPage = errors.New("empty page: ")

// mainBindingRaw returns the primary list binding (items/comments/users/
// members) declared by a contract.
func mainBindingRaw(c *contracts.Contract) (kind, raw string) {
	switch {
	case c.Binding.Items != "":
		return "items", c.Binding.Items
	case c.Binding.Comments != "":
		return "comments", c.Binding.Comments
	case c.Binding.Users != "":
		return "users", c.Binding.Users
	case c.Binding.Members != "":
		return "members", c.Binding.Members
	}
	return "", ""
}

// checkBindings fails closed when the contract's primary binding is missing
// or resolves to a non-list value. A JSON null binding (douyin's zero-comment
// shape, `{"comments": null}`) and an empty list are both VALID zero-record
// pages: the walk ends cleanly instead of surfacing ErrEmptyPage — that error
// is a rotation trigger under auto account mode, and a genuinely comment-less
// item must never burn accounts (report G3).
func checkBindings(c *contracts.Contract, doc map[string]any) error {
	kind, raw := mainBindingRaw(c)
	if raw == "" {
		return nil // fields-only contract (e.g. meta): nothing to enforce
	}
	p, err := contracts.ParsePath(raw)
	if err != nil {
		return fmt.Errorf("bad binding %q: %w", raw, err)
	}
	vs := p.Select(doc)
	if len(vs) == 0 {
		return fmt.Errorf("%s binding %q missing from response", kind, raw)
	}
	if vs[0] == nil {
		return nil // explicit null = clean empty page (zero comments/items)
	}
	if _, ok := vs[0].([]any); !ok {
		return fmt.Errorf("%s binding %q is not a list", kind, raw)
	}
	// Empty list = valid zero-record page; missing path / non-list stays an error.
	return nil
}

// selectRecords flattens a binding path selection into element maps.
func selectRecords(p contracts.Path, doc map[string]any) []map[string]any {
	var out []map[string]any
	for _, v := range p.Select(doc) {
		switch t := v.(type) {
		case []any:
			for _, el := range t {
				if m, ok := el.(map[string]any); ok {
					out = append(out, m)
				}
			}
		case map[string]any:
			out = append(out, t)
		}
	}
	return out
}

// nextCursor derives the next pagination position from a response document.
// HasMore defaults to false (single page) when the contract declares no
// has_more path.
func (e *Engine) nextCursor(c *contracts.Contract, doc map[string]any, cur model.Cursor) model.Cursor {
	nxt := model.Cursor{Page: cur.Page + 1}
	if len(cur.Source) > 0 {
		nxt.Source = make(map[string]any, len(cur.Source))
		for k, v := range cur.Source {
			nxt.Source[k] = v
		}
	}
	if c.Paging.HasMorePath != "" {
		if p, err := contracts.ParsePath(c.Paging.HasMorePath); err == nil {
			if v := p.First(doc); v != nil {
				nxt.HasMore = asBool(v)
			}
		}
	}
	if c.Paging.NextCursorPath != "" {
		if p, err := contracts.ParsePath(c.Paging.NextCursorPath); err == nil {
			if v := p.First(doc); v != nil {
				if nxt.Source == nil {
					nxt.Source = map[string]any{}
				}
				nxt.Source["cursor"] = v
				// Sentinel (report item 11): platforms terminate cursored
				// walks with an explicit end marker (kuaishou "no_more").
				// The old code only stopped because the numeric-string
				// coincidence made asBool false — recognize the sentinel
				// explicitly instead of relying on the accident.
				if s, ok := v.(string); ok && isCursorSentinel(s) {
					nxt.HasMore = false
				}
			}
		}
	}
	return nxt
}

// cursorSentinels are the explicit end-of-list markers platforms embed in
// the next-cursor field (case-insensitive).
var cursorSentinels = map[string]bool{
	"no_more": true,
}

// isCursorSentinel reports whether a cursor value is an explicit end marker.
func isCursorSentinel(v string) bool {
	return cursorSentinels[strings.ToLower(strings.TrimSpace(v))]
}

// fetchPages runs the pagination loop for one contract: cursor and count
// params per page (limit -> count), has_more driven continuation, and final
// limit truncation. Pages stop when the contract declares no next-cursor
// path (single page) or has_more is false. The optional backtrack predicate
// lives in predicate.go (fetchPagesWith); this entry point keeps the
// predicate-free legacy behavior.
func (e *Engine) fetchPages(ctx context.Context, name string, pathParams, baseQuery map[string]string, cur model.Cursor, limit int) ([]map[string]any, model.Cursor, error) {
	return e.fetchPagesWith(ctx, name, pathParams, baseQuery, cur, limit, nil)
}

func truncate(recs []map[string]any, limit int) []map[string]any {
	if limit > 0 && len(recs) > limit {
		return recs[:limit]
	}
	return recs
}

// SearchItems collects keyword search results for a platform. filter
// (mediaTypeFilter) post-filters items by MediaType when non-empty.
func (e *Engine) SearchItems(ctx context.Context, platform, keyword, filter string, cur model.Cursor, limit int) ([]model.Item, model.Cursor, error) {
	name, err := e.resolveName(platform, "search")
	if err != nil {
		return nil, cur, err
	}
	pathParams := map[string]string{"keyword": keyword}
	query := map[string]string{}
	if filter != "" {
		query["type"] = filter
	}
	recs, nxt, err := e.fetchPages(ctx, name, pathParams, query, cur, limit)
	c, _ := e.reg.Get(name)
	items := make([]model.Item, 0, len(recs))
	for _, r := range recs {
		items = append(items, bindItem(c, r))
	}
	// Partial-data semantics (report t03): a late-page failure still returns
	// the records already collected alongside the error — callers flush them
	// to disk before exiting.
	if err != nil {
		return items, nxt, err
	}
	if filter != "" {
		f := items[:0]
		for _, it := range items {
			if it.MediaType == filter {
				f = append(f, it)
			}
		}
		items = f
	}
	return items, nxt, nil
}

// firstPlaceholder returns the contract's primary identity placeholder (the
// item/group id param name), falling back to def when none is declared.
func firstPlaceholder(c *contracts.Contract, def string) string {
	if len(c.Transport.Placeholders) > 0 {
		return c.Transport.Placeholders[0]
	}
	return def
}

// ItemComments collects the comment list of one item, paginated.
func (e *Engine) ItemComments(ctx context.Context, platform, itemID string, cur model.Cursor, limit int) ([]model.Comment, model.Cursor, error) {
	name, err := e.resolveName(platform, "comments")
	if err != nil {
		return nil, cur, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return nil, cur, fmt.Errorf("collect: contract %q not registered", name)
	}
	recs, nxt, err := e.fetchPages(ctx, name, map[string]string{firstPlaceholder(c, "item_id"): itemID}, nil, cur, limit)
	cmts := make([]model.Comment, 0, len(recs))
	for _, r := range recs {
		cmts = append(cmts, bindComment(c, r))
	}
	if err != nil {
		return cmts, nxt, err // partial data + error (flush-before-exit)
	}
	// Payload + user-enrich combination (capability #2): complete each
	// unique author's twelve-field profile through the platform user face.
	e.enrichCommenters(ctx, platform, cmts)
	return cmts, nxt, nil
}

// CommentReplies collects the replies of one top-level comment, paginated.
// The comment id travels as the contract's first declared placeholder — the
// reply-target parameter name is contract data, so each platform carries its
// real-world name (douyin "comment_id", xhs "root_comment_id" per the A-line
// corpus verdict 64/64); the "comment_id" fallback only covers contracts
// that declare no placeholders at all.
func (e *Engine) CommentReplies(ctx context.Context, platform, itemID, cid string, cur model.Cursor, limit int) ([]model.Comment, model.Cursor, error) {
	name, err := e.resolveName(platform, "replies")
	if err != nil {
		return nil, cur, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return nil, cur, fmt.Errorf("collect: contract %q not registered", name)
	}
	recs, nxt, err := e.fetchPages(ctx, name, map[string]string{firstPlaceholder(c, "comment_id"): cid}, nil, cur, limit)
	cmts := make([]model.Comment, 0, len(recs))
	for _, r := range recs {
		cmts = append(cmts, bindComment(c, r))
	}
	if err != nil {
		return cmts, nxt, err // partial data + error (flush-before-exit)
	}
	// Replies carry the same twelve-field author contract as comments.
	e.enrichCommenters(ctx, platform, cmts)
	return cmts, nxt, nil
}

// UserProfile resolves one author/actor profile by sec_uid.
func (e *Engine) UserProfile(ctx context.Context, platform, secUID string) (model.UserProfile, error) {
	name, err := e.resolveName(platform, "user")
	if err != nil {
		return model.UserProfile{}, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return model.UserProfile{}, fmt.Errorf("collect: contract %q not registered", name)
	}
	recs, _, err := e.fetchPages(ctx, name, map[string]string{"sec_uid": secUID}, nil, model.Cursor{}, 1)
	if err != nil {
		return model.UserProfile{}, err
	}
	if len(recs) == 0 {
		return model.UserProfile{}, fmt.Errorf("collect %s: no user record bound", name)
	}
	return bindUser(c, recs[0]), nil
}

// GroupMembers enumerates the members of a group (silent enumeration),
// paginated.
func (e *Engine) GroupMembers(ctx context.Context, platform, groupID string, cur model.Cursor, limit int) ([]model.GroupMember, model.Cursor, error) {
	name, err := e.resolveName(platform, "group")
	if err != nil {
		return nil, cur, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return nil, cur, fmt.Errorf("collect: contract %q not registered", name)
	}
	recs, nxt, err := e.fetchPages(ctx, name, map[string]string{firstPlaceholder(c, "group_id"): groupID}, nil, cur, limit)
	members := make([]model.GroupMember, 0, len(recs))
	for _, r := range recs {
		members = append(members, bindMember(c, r, groupID))
	}
	if err != nil {
		return members, nxt, err // partial data + error (flush-before-exit)
	}
	return members, nxt, nil
}

// SendResult is the outcome of one direct-message send operation.
type SendResult struct {
	MsgID  string         `json:"msg_id"`
	Status string         `json:"status"`
	Raw    map[string]any `json:"raw,omitempty"`
}

// SendMessage sends one direct message to a target user (sec_uid) using the
// platform's send-message contract. The text and target travel in the JSON
// body. This is a single-shot action (not paginated); fail-closed on the
// contract's signature/cookie requirements like every other fetch.
func (e *Engine) SendMessage(ctx context.Context, platform, secUID, text string) (SendResult, error) {
	name, err := e.resolveName(platform, "send_message")
	if err != nil {
		return SendResult{}, err
	}
	if _, ok := e.reg.Get(name); !ok {
		return SendResult{}, fmt.Errorf("collect: contract %q not registered", name)
	}
	// The JSON body is assembled by buildURL from transport.Body + caller
	// query fields (sec_user_id, text); the target and text travel in the body.
	doc, err := e.Fetch(ctx, name, nil, map[string]string{"sec_user_id": secUID, "text": text})
	if err != nil {
		return SendResult{}, err
	}
	var res SendResult
	res.Raw = doc
	if doc["data"] != nil {
		if d, ok := doc["data"].(map[string]any); ok {
			res.MsgID, _ = d["msg_id"].(string)
			res.Status, _ = d["status"].(string)
		}
	}
	return res, nil
}

// VideoMeta is the resolved watermark-free play address of one item.
type VideoMeta struct {
	AwemeID string `json:"aweme_id"`
	URL     string `json:"url"`
	Cover   string `json:"cover"`
	Bytes   int64  `json:"bytes,omitempty"`
}

// firstURL returns the first URL string from a nested value that is either a
// []string, []any, or a plain string.
func firstURL(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		for _, el := range t {
			if s, ok := el.(string); ok && s != "" {
				return s
			}
		}
	case []string:
		for _, s := range t {
			if s != "" {
				return s
			}
		}
	}
	return ""
}

// docField resolves a contract binding field against the whole response
// document via the JSONPath walker (binding paths are document-level).
func docField(c *contracts.Contract, name string, doc map[string]any) any {
	raw := c.Binding.Fields[name]
	if raw == "" {
		return nil
	}
	p, err := contracts.ParsePath(raw)
	if err != nil {
		return nil
	}
	return p.First(doc)
}

// ResolveVideo resolves one item's watermark-free play URL from the
// video-download contract: play URL, cover and video id all resolve through
// the contract's binding fields (play_url / cover / aweme_id). Fail-closed:
// a contract without a play_url binding errors instead of the engine
// guessing platform-specific response paths.
func (e *Engine) ResolveVideo(ctx context.Context, platform, itemID string) (VideoMeta, error) {
	name, err := e.resolveName(platform, "video_download")
	if err != nil {
		return VideoMeta{}, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return VideoMeta{}, fmt.Errorf("collect: contract %q not registered", name)
	}
	if c.Binding.Fields["play_url"] == "" {
		return VideoMeta{}, fmt.Errorf("collect %s: contract declares no binding field %q", name, "play_url")
	}
	doc, err := e.Fetch(ctx, name, map[string]string{firstPlaceholder(c, "aweme_id"): itemID}, nil)
	if err != nil {
		return VideoMeta{}, err
	}
	var meta VideoMeta
	meta.URL = firstURL(docField(c, "play_url", doc))
	if meta.URL == "" {
		return VideoMeta{}, fmt.Errorf("collect: no play URL found for %q", itemID)
	}
	meta.AwemeID = itemID
	if id := asStr(docField(c, "aweme_id", doc)); id != "" {
		meta.AwemeID = id
	}
	meta.Cover = firstURL(docField(c, "cover", doc))
	return meta, nil
}

// Download streams a URL to dst using the engine's HTTP client (the actual
// bytes are a plain GET; platform signing is irrelevant for the CDN URL).
// The body is streamed via io.Copy, never buffered whole in memory.
func (e *Engine) Download(ctx context.Context, url string, dst io.Writer) (int64, error) {
	if url == "" {
		return 0, errors.New("collect: empty download url")
	}
	status, stream, err := e.hc.WithContract("video_download").DoStream(ctx, http.MethodGet, url, nil, nil)
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	if status < 200 || status >= 300 {
		return 0, fmt.Errorf("video download: status %d", status)
	}
	return io.Copy(dst, stream)
}

// CollectFolders lists a user's bookmark/collects folders (cursor-paginated).
func (e *Engine) CollectFolders(ctx context.Context, platform string, cur model.Cursor, limit int) ([]model.Item, model.Cursor, error) {
	name, err := e.resolveName(platform, "collects")
	if err != nil {
		return nil, cur, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return nil, cur, fmt.Errorf("collect: contract %q not registered", name)
	}
	recs, nxt, err := e.fetchPages(ctx, name, nil, nil, cur, limit)
	items := make([]model.Item, 0, len(recs))
	for _, r := range recs {
		items = append(items, bindItem(c, r))
	}
	if err != nil {
		return items, nxt, err // partial data + error (flush-before-exit)
	}
	return items, nxt, nil
}

// CollectVideos lists the videos inside one bookmark folder (cursor-paginated).
func (e *Engine) CollectVideos(ctx context.Context, platform, collectsID string, cur model.Cursor, limit int) ([]model.Item, model.Cursor, error) {
	name, err := e.resolveName(platform, "collects_videos")
	if err != nil {
		return nil, cur, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return nil, cur, fmt.Errorf("collect: contract %q not registered", name)
	}
	recs, nxt, err := e.fetchPages(ctx, name, map[string]string{firstPlaceholderFrom(e.reg, name, "collects_id"): collectsID}, nil, cur, limit)
	items := make([]model.Item, 0, len(recs))
	for _, r := range recs {
		items = append(items, bindItem(c, r))
	}
	if err != nil {
		return items, nxt, err // partial data + error (flush-before-exit)
	}
	return items, nxt, nil
}

// IMUnread is the result of an unread-count poll.
type IMUnread struct {
	TotalUnread   int64            `json:"total_unread"`
	Conversations []map[string]any `json:"conversations,omitempty"`
}

// FetchIMUnread polls the IM unread-count endpoint.
func (e *Engine) FetchIMUnread(ctx context.Context, platform string) (IMUnread, error) {
	name, err := e.resolveName(platform, "im_unread")
	if err != nil {
		return IMUnread{}, err
	}
	doc, err := e.Fetch(ctx, name, nil, nil)
	if err != nil {
		return IMUnread{}, err
	}
	c, _ := e.reg.Get(name)
	var res IMUnread
	res.TotalUnread = fieldInt(c, "total_unread", doc, []string{"unread_count", "total_unread"})
	for _, key := range []string{"conv_list", "conversation_list"} {
		if arr, ok := doc[key].([]any); ok {
			for _, el := range arr {
				if m, ok := el.(map[string]any); ok {
					res.Conversations = append(res.Conversations, m)
				}
			}
			break
		}
	}
	return res, nil
}

// firstPlaceholderFrom returns the contract's first placeholder, falling back to def.
func firstPlaceholderFrom(reg *contracts.Registry, name, def string) string {
	c, ok := reg.Get(name)
	if !ok {
		return def
	}
	return firstPlaceholder(c, def)
}
