// impersonate.go — TLS/HTTP2 指纹拟态传输层（ADR-0100）。
//
// MEDIAMON_TLS_IMPERSONATE=chrome152 时，Client.Do 走 bogdanfinn/tls-client
// （utls ClientHello + fhttp H2 指纹 = 真 Chrome 152），替代 stdlib 传输；
// 重试/退避/Retry-After 语义与 stdlib 路径一致。Cookie 由引擎按账号以请求头
// 显式携带，拟态客户端不设 jar（避免双源）。
package httpclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// fhttp 类型别名（fhttp 是 net/http 的指纹控 fork，类型不通用）。
type (
	fhttpRequest  = fhttp.Request
	fhttpResponse = fhttp.Response
)

// parseSignURL parses the raw URL, enforces the scheme, and runs the
// contract signer over the existing query params (shared by stdlib and
// impersonation paths) — the merged query is what gets sent.
func parseSignURL(c *Client, rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("httpclient: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("httpclient: unsupported scheme %q", u.Scheme)
	}
	if c.signer != nil {
		params := make(map[string]string)
		for k, vs := range u.Query() {
			if len(vs) > 0 {
				params[k] = vs[len(vs)-1]
			}
		}
		sig, serr := c.signer.Sign(context.Background(), c.contract, u.String(), params)
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
	return u, nil
}

// impersonateProfile resolves the ADR-0100 profile from the env var. Empty
// (default) disables impersonation entirely.
func impersonateProfile() string {
	return strings.TrimSpace(os.Getenv("MEDIAMON_TLS_IMPERSONATE"))
}

func profileByName(name string) (profiles.ClientProfile, bool) {
	switch strings.ToLower(name) {
	case "chrome152", "chrome":
		return profiles.Chrome_152, true
	case "chrome150":
		return profiles.Chrome_150, true
	case "chrome146":
		return profiles.Chrome_146, true
	}
	return profiles.ClientProfile{}, false
}

// impersonateDo performs one request through the tls-client transport with
// stdlib-equivalent retry semantics. It mirrors Client.Do's loop because the
// fhttp request type differs from net/http.
func (c *Client) impersonateDo(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (int, []byte, time.Duration, error) {
	cl := impersonateFor(c)
	if cl == nil {
		if err := c.buildImpersonate(); err != nil {
			return 0, nil, 0, err
		}
		cl = impersonateFor(c)
		if cl == nil {
			return 0, nil, 0, fmt.Errorf("httpclient: impersonate init failed")
		}
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
				return 0, nil, 0, fmt.Errorf("httpclient: %w", ctx.Err())
			case <-time.After(c.backoffFor(i, lastRetryAfter)):
			}
		}
		status, rb, ra, err := c.impersonateOnce(ctx, method, rawURL, headers, body)
		if err != nil {
			return 0, nil, 0, err
		}
		lastStatus, lastBody, lastRetryAfter = status, rb, ra
		if status == http.StatusTooManyRequests || status >= 500 {
			continue
		}
		return status, rb, ra, nil
	}
	return lastStatus, lastBody, lastRetryAfter,
		fmt.Errorf("httpclient(impersonate): %s %s failed after %d attempt(s), last status %d", method, rawURL, attempts, lastStatus)
}

func (c *Client) impersonateOnce(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (int, []byte, time.Duration, error) {
	cl := impersonateFor(c) // impersonateDo 已确保构建完成
	u, err := parseSignURL(c, rawURL)
	if err != nil {
		return 0, nil, 0, err
	}
	req, err := fhttp.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, fmt.Errorf("httpclient: new request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.UA())
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("httpclient: do: %w", err)
	}
	defer resp.Body.Close()
	var rdr io.Reader = resp.Body
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

// buildImpersonate lazily constructs the tls-client with the resolved Chrome
// profile (ADR-0100: Chrome_152 default, matches the pinned account UA).
func (c *Client) buildImpersonate() error {
	name := c.impersonateName
	if name == "" {
		return fmt.Errorf("httpclient: impersonation disabled")
	}
	prof, ok := profileByName(name)
	if !ok {
		return fmt.Errorf("httpclient: unknown impersonate profile %q", name)
	}
	timeout := int(c.cfg.Timeout / time.Second)
	if timeout <= 0 {
		timeout = 90
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(timeout),
		tls_client.WithClientProfile(prof),
		tls_client.WithNotFollowRedirects(), // stdlib 路径同样不跟随（引擎按状态族处置）
	}
	cl, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return fmt.Errorf("httpclient: tls-client init: %w", err)
	}
	if c.cfg.Proxy != "" {
		if err := cl.SetProxy(c.cfg.Proxy); err != nil {
			return fmt.Errorf("httpclient: proxy: %w", err)
		}
	}
	impersonateStore.Store(c, cl)
	return nil
}

// impersonateStore caches the tls-client per *Client pointer (lazy singleton;
// Client is used single-threaded per fetch chain, guarded by the mutex for
// the rotated-clone case).
var impersonateStore sync.Map

func impersonateFor(c *Client) tls_client.HttpClient {
	if v, ok := impersonateStore.Load(c); ok {
		return v.(tls_client.HttpClient)
	}
	return nil
}
