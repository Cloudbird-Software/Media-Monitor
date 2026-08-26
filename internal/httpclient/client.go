// Package httpclient is a thin HTTP client for the media collectors: it adds
// a rotating User-Agent pool, retry with exponential backoff on 429/5xx, and
// an injectable request signer that can attach query parameters (e.g. a_bogus
// / msToken) computed per contract. There is no cookie jar on purpose —
// cookies are stateless and travel via the caller-supplied headers.
package httpclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// Config tunes a Client.
type Config struct {
	// BaseHeaders are merged into every request first (caller headers win).
	BaseHeaders map[string]string
	// UserAgents is the rotation pool; a built-in pool of generic desktop
	// and mobile UAs is used when empty.
	UserAgents []string
	// Timeout per request (0 selects a 30s default).
	Timeout time.Duration
	// MaxRetries is the number of extra attempts after the first one for
	// 429/Too Many Requests and 5xx responses. 0 means a single attempt.
	MaxRetries int
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

// UA returns the next User-Agent in the pool (round-robin rotation).
func (c *Client) UA() string {
	return c.uaPool[int(c.uaIdx.Add(1)-1)%len(c.uaPool)]
}

// Do performs one request with retry: transport errors and non-429/5xx
// responses return immediately; 429/5xx responses retry with exponential
// backoff starting at 100ms, honoring ctx cancellation between attempts.
// Returns the last HTTP status and body (last attempt on exhaustion).
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
	for i := 0; i < attempts; i++ {
		if i > 0 {
			backoff := time.Duration(100*time.Millisecond) << uint(i-1)
			select {
			case <-ctx.Done():
				return 0, nil, fmt.Errorf("httpclient: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}
		status, rb, err := c.doOnce(ctx, method, rawURL, headers, body)
		if err != nil {
			return 0, nil, err
		}
		lastStatus, lastBody = status, rb
		if status == http.StatusTooManyRequests || status >= 500 {
			continue
		}
		return status, rb, nil
	}
	return lastStatus, lastBody,
		fmt.Errorf("httpclient: %s %s failed after %d attempt(s), last status %d", method, rawURL, attempts, lastStatus)
}

func (c *Client) doOnce(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (int, []byte, error) {
	resp, err := c.doRequest(ctx, method, rawURL, headers, body)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("httpclient: read body: %w", err)
	}
	return resp.StatusCode, rb, nil
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

// defaultUAs is a pool of generic consumer User-Agents (desktop + mobile
// WebKit/Gecko). No private or internal domains appear here.
var defaultUAs = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36 Edg/123.0.0.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36",
	"Mozilla/5.0 (Linux; Android 13; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Mobile Safari/537.36",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
	"Mozilla/5.0 (iPad; CPU OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1",
	"Mozilla/5.0 (Linux; Android 12; M2012K11AC) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Mobile Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:125.0) Gecko/20100101 Firefox/125.0",
}
