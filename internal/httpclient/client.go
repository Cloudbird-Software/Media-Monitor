// Package httpclient is a thin HTTP client for the media collectors: it adds
// a rotating User-Agent pool, retry with exponential backoff on 429/5xx, and
// an injectable request signer that can attach query parameters (e.g. a_bogus
// / msToken) computed per contract. Cookies are stateless by default and
// travel via the caller-supplied headers; Session() clones get a real cookie
// jar so server Set-Cookie rotations persist within one identity.
package httpclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Config tunes a Client.
type Config struct {
	// BaseHeaders are merged into every request first (caller headers win).
	BaseHeaders map[string]string
	// UserAgents is the rotation pool; a built-in pool of real desktop
	// Chrome/Edge UAs is used when empty.
	UserAgents []string
	// Timeout per request (0 selects a 30s default).
	Timeout time.Duration
	// MaxRetries is the number of extra attempts after the first one for
	// 429/Too Many Requests and 5xx responses. 0 means a single attempt.
	MaxRetries int
	// RetryBase is the exponential backoff base for retries (0 = 250ms).
	// Each retry waits RetryBase·2^(n-1) with ±20% jitter (silent-scraping
	// A2/E: constant-cadence error storms are a bot signature), and never
	// less than a server-sent Retry-After (capped at 30s).
	RetryBase time.Duration
	// Proxy is an optional proxy URL (e.g. "http://user:pass@host:port",
	// "socks5://host:port"). When set, every request is routed through it.
	Proxy string
}

// Signer computes per-contract request signature values. params holds the
// query parameters already present on the URL. The returned map is merged
// into the outgoing query string.
type Signer interface {
	Sign(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error)
}

// DefaultMaxRetries is the silent-scraping default for collect traffic
// (report item 5 / E: MaxRetries=0 meant a single attempt and an instant
// error storm). Override with MEDIAMON_MAX_RETRIES.
const DefaultMaxRetries = 2

// MaxRetriesFromEnv resolves the retry budget: MEDIAMON_MAX_RETRIES wins
// (0 = single attempt, the legacy explicit opt-out), default 2.
func MaxRetriesFromEnv() int {
	v := strings.TrimSpace(os.Getenv("MEDIAMON_MAX_RETRIES"))
	if v == "" {
		return DefaultMaxRetries
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return DefaultMaxRetries
	}
	return n
}

// StaticSigner adapts a plain function to the Signer interface.
type StaticSigner struct {
	Fn func(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error)
}

// Sign implements Signer.
func (s StaticSigner) Sign(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error) {
	if s.Fn == nil {
		return nil, nil
	}
	return s.Fn(ctx, contractName, url, params)
}

// Client sends HTTP requests with the configured policy. Do is safe for
// concurrent use; WithSigner/WithContract must be called before the client
// is shared.
type Client struct {
	cfg      Config
	hc       *http.Client
	signer   Signer
	contract string // contract name forwarded to the signer
	uaPool   []string
	uaIdx    atomic.Uint64
}

// New builds a Client; defaulting Timeout and the UA pool where needed.
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if len(cfg.UserAgents) == 0 {
		cfg.UserAgents = append([]string(nil), defaultUAs...)
	}
	hc := &http.Client{Timeout: cfg.Timeout}
	if cfg.Proxy != "" {
		if u, err := url.Parse(cfg.Proxy); err == nil {
			hc.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
		}
	}
	return &Client{
		cfg:    cfg,
		hc:     hc,
		uaPool: cfg.UserAgents,
	}
}

// WithSigner attaches a signer and returns the client for chaining.
func (c *Client) WithSigner(s Signer) *Client {
	c.signer = s
	return c
}

// WithContract names the contract these requests belong to; the name is
// forwarded to the signer on every Do call.
func (c *Client) WithContract(name string) *Client {
	c.contract = name
	return c
}

// Session returns a clone backed by its own cookie jar (sharing the config,
// signer, UA pool and transport of the receiver). Use one Session per
// identity/cookie-lifetime so Set-Cookie rotations (msToken, ttwid refresh)
// persist within the session and never leak across identities. Caller-set
// Cookie headers are still honored — jar cookies are appended after them.
func (c *Client) Session() *Client {
	// Field-wise clone: a plain struct copy would duplicate the atomic UA
	// counter by value (go vet: assignment copies lock value); the session
	// starts its own rotation instead.
	nc := &Client{
		cfg:      c.cfg,
		signer:   c.signer,
		contract: c.contract,
		uaPool:   c.uaPool,
	}
	base := &http.Client{Timeout: c.hc.Timeout, Transport: c.hc.Transport}
	if jar, err := cookiejar.New(nil); err == nil {
		base.Jar = jar
	}
	nc.hc = base
	return nc
}

// UA returns the next User-Agent in the pool (round-robin rotation).
func (c *Client) UA() string {
	return c.uaPool[int(c.uaIdx.Add(1)-1)%len(c.uaPool)]
}

// Do performs one request with retry: transport errors and non-429/5xx
// responses return immediately; 429/5xx responses retry with exponential
// backoff plus ±20% jitter (never below a server Retry-After, capped at
// 30s), honoring ctx cancellation between attempts. Returns the last HTTP
// status and body (last attempt on exhaustion).
func (c *Client) Do(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if method == "" {
		method = http.MethodGet
	}
	attempts := c.cfg.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastStatus int
	var lastBody []byte
	var lastRetryAfter time.Duration
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return 0, nil, fmt.Errorf("httpclient: %w", ctx.Err())
			case <-time.After(c.backoffFor(i, lastRetryAfter)):
			}
		}
		status, rb, ra, err := c.doOnce(ctx, method, rawURL, headers, body)
		if err != nil {
			return 0, nil, err
		}
		lastStatus, lastBody, lastRetryAfter = status, rb, ra
		if status == http.StatusTooManyRequests || status >= 500 {
			continue
		}
		return status, rb, nil
	}
	return lastStatus, lastBody,
		fmt.Errorf("httpclient: %s %s failed after %d attempt(s), last status %d", method, rawURL, attempts, lastStatus)
}

// retryJitter spreads the ±20% jitter factor for backoffFor.
func retryJitterFactor() float64 {
	return 0.8 + 0.4*rand.Float64()
}

// backoffFor computes attempt n's wait: RetryBase·2^(n-1) scaled by ±20%
// jitter, raised to the server's Retry-After when larger, capped at 30s.
func (c *Client) backoffFor(attempt int, retryAfter time.Duration) time.Duration {
	base := c.cfg.RetryBase
	if base <= 0 {
		base = 250 * time.Millisecond
	}
	d := time.Duration(float64(base<<uint(attempt-1)) * retryJitterFactor())
	if retryAfter > d {
		d = retryAfter
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// parseRetryAfter converts a Retry-After header value (delta-seconds or
// HTTP-date) into a duration; ok=false when absent/invalid. The result is
// clamped to [0, 30s] — a hostile/huge value must not stall the client.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, true
		}
		d := time.Duration(secs) * time.Second
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		return d, true
	}
	if ts, err := http.ParseTime(v); err == nil {
		d := ts.Sub(now)
		if d < 0 {
			d = 0
		}
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		return d, true
	}
	return 0, false
}

// doOnce performs one attempt and also surfaces the server's Retry-After
// (parsed against the attempt time) for the retry loop.
func (c *Client) doOnce(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (int, []byte, time.Duration, error) {
	resp, err := c.doRequest(ctx, method, rawURL, headers, body)
	if err != nil {
		return 0, nil, 0, err
	}
	defer resp.Body.Close()
	var rdr io.Reader = resp.Body
	// gzip answers to a manually-set Accept-Encoding: when the caller (or
	// the merged browser header sets) declares Accept-Encoding, Go's
	// transport passes it verbatim and does NOT transparently decompress —
	// yet the xhs/ks surfaces answer gzip to exactly that offer (corpus
	// truth; the synth data face models it, final-audit P3 e2e alignment
	// exposed it). The transport strips Content-Encoding when it decompressed
	// by itself, so the header's presence is the precise manual case. Only
	// gzip is handled; anything else stays raw and fails loud at the parse
	// boundary (no silent wrong data). DoStream (media bytes) is untouched.
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, zerr := gzip.NewReader(resp.Body)
		if zerr != nil {
			return 0, nil, 0, fmt.Errorf("httpclient: gzip body: %w", zerr)
		}
		defer zr.Close()
		rdr = zr
	}
	rb, err := io.ReadAll(rdr)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("httpclient: read body: %w", err)
	}
	ra, _ := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	return resp.StatusCode, rb, ra, nil
}

// DoStream performs a single-attempt request and returns the response body
// as an open stream (the caller must close it). Unlike Do it never buffers
// the body in memory, which makes it suitable for large media downloads;
// retries are the caller's decision (a partial stream cannot be replayed).
func (c *Client) DoStream(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (int, io.ReadCloser, error) {
	resp, err := c.doRequest(ctx, method, rawURL, headers, body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, resp.Body, nil
}

// doRequest builds and sends one request: signer decoration, base+caller
// headers and the rotating UA (a caller-supplied UA header wins).
func (c *Client) doRequest(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if method == "" {
		method = http.MethodGet
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("httpclient: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("httpclient: unsupported scheme %q", u.Scheme)
	}

	// Signer sees the URL's existing query as a param map, then its output
	// is merged into the query before the request is sent.
	if c.signer != nil {
		params := make(map[string]string)
		for k, vs := range u.Query() {
			if len(vs) > 0 {
				params[k] = vs[len(vs)-1]
			}
		}
		sig, serr := c.signer.Sign(ctx, c.contract, u.String(), params)
		if serr != nil {
			return nil, fmt.Errorf("httpclient: sign %q: %w", c.contract, serr)
		}
		if len(sig) > 0 {
			q := u.Query()
			for k, v := range sig {
				q.Set(k, v)
			}
			u.RawQuery = q.Encode()
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("httpclient: new request: %w", err)
	}
	h := make(http.Header, len(c.cfg.BaseHeaders)+len(headers)+1)
	for k, v := range c.cfg.BaseHeaders {
		h.Set(k, v)
	}
	for k, v := range headers {
		h.Set(k, v)
	}
	if h.Get("User-Agent") == "" {
		h.Set("User-Agent", c.UA())
	}
	req.Header = h

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// defaultUAs is the built-in fallback pool: real, currently-existing desktop
// Chrome/Edge User-Agents (majors 148-152 — the recorded human baseline runs
// Chrome 152). Deployments should inject the fuller accounts UA pool via
// Config.UserAgents. No fabricated version numbers (silent-scraping B2).
var defaultUAs = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36 Edg/151.0.0.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
}
