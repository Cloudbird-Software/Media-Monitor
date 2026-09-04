package httpclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

// TestDoRetry429Then200: the server fails 429 twice then succeeds; retry
// recovers and the caller sees the 200 body.
func TestDoRetry429Then200(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("slow down"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok-body"))
	}))
	defer srv.Close()

	c := New(Config{MaxRetries: 5, Timeout: 2 * time.Second})
	status, body, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if string(body) != "ok-body" {
		t.Fatalf("body = %q, want %q", body, "ok-body")
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("server saw %d requests, want 3", n)
	}
}

// TestDo5xxNeverSucceeds: a permanently 5xx endpoint exhausts the retry
// budget and returns the last status plus an error.
func TestDo5xxNeverSucceeds(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("down"))
	}))
	defer srv.Close()

	c := New(Config{MaxRetries: 2, Timeout: 2 * time.Second})
	status, body, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, nil)
	if err == nil {
		t.Fatal("Do succeeded, want error after retry exhaustion")
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if string(body) != "down" {
		t.Fatalf("body = %q, want %q", body, "down")
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("server saw %d requests, want 3 (1 + MaxRetries)", n)
	}
}

// TestDoEchoBodyAndHeaders: caller headers override BaseHeaders, UA comes
// from the configured pool, and the body round-trips.
func TestDoEchoBodyAndHeaders(t *testing.T) {
	var gotUA, gotBase, gotCaller string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotBase = r.Header.Get("X-Base")
		gotCaller = r.Header.Get("X-Caller")
		buf, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf)
	}))
	defer srv.Close()

	c := New(Config{
		BaseHeaders: map[string]string{"X-Base": "base-value"},
		UserAgents:  []string{"ua-pool-A", "ua-pool-B"},
		Timeout:     2 * time.Second,
	})
	const payload = `{"hello":"world"}`
	status, body, err := c.Do(context.Background(), http.MethodPost, srv.URL,
		map[string]string{"X-Base": "overridden", "X-Caller": "caller-value"}, []byte(payload))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK || string(body) != payload {
		t.Fatalf("echo mismatch: status=%d body=%q", status, body)
	}
	if gotBase != "overridden" {
		t.Fatalf("X-Base = %q, want caller override", gotBase)
	}
	if gotCaller != "caller-value" {
		t.Fatalf("X-Caller = %q", gotCaller)
	}
	if gotUA != "ua-pool-A" {
		t.Fatalf("User-Agent = %q, want first pool entry", gotUA)
	}
}

// TestUARotation: UA rotates round-robin through the pool, and the built-in
// pool is at least 8 entries.
func TestUARotation(t *testing.T) {
	c := New(Config{UserAgents: []string{"ua-1", "ua-2", "ua-3"}})
	want := []string{"ua-1", "ua-2", "ua-3", "ua-1"}
	for i, w := range want {
		if got := c.UA(); got != w {
			t.Fatalf("UA call %d = %q, want %q", i, got, w)
		}
	}
	def := New(Config{})
	seen := map[string]bool{}
	for i := 0; i < len(defaultUAs)+3; i++ {
		u := def.UA()
		seen[u] = true
	}
	if len(seen) < len(defaultUAs) {
		t.Fatalf("UA pool rotation only produced %d distinct UAs, pool has %d", len(seen), len(defaultUAs))
	}
	if len(defaultUAs) < 8 {
		t.Fatalf("default UA pool has %d entries, want >= 8", len(defaultUAs))
	}
	for _, u := range defaultUAs {
		if strings.Contains(u, "Cloudbird") || strings.Contains(u, "localhost") {
			t.Fatalf("default UA contains internal marker: %q", u)
		}
	}
}

// TestSignerCalledAndMerged: the signer receives the URL query as params plus
// the contract name, and its returned kv lands in the outgoing query.
func TestSignerCalledAndMerged(t *testing.T) {
	type sigCall struct {
		contract, url string
		params        map[string]string
	}
	var callCh = make(chan sigCall, 1)
	var sawQuery atomic.Value // string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery.Store(r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	signer := StaticSigner{Fn: func(ctx context.Context, contract, rawURL string, params map[string]string) (map[string]string, error) {
		callCh <- sigCall{contract: contract, url: rawURL, params: params}
		return map[string]string{"a_bogus": "sig-value", "msToken": "token-abc"}, nil
	}}
	c := New(Config{Timeout: 2 * time.Second, UserAgents: []string{"ua"}}).
		WithSigner(signer).WithContract("douyin-search")

	target := srv.URL + "?device_platform=webapp&aid=6383"
	status, _, err := c.Do(context.Background(), http.MethodGet, target, nil, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	sc := <-callCh
	if sc.contract != "douyin-search" {
		t.Fatalf("signer contract = %q", sc.contract)
	}
	if sc.params["device_platform"] != "webapp" || sc.params["aid"] != "6383" {
		t.Fatalf("signer params = %v", sc.params)
	}

	q := sawQuery.Load().(string)
	if !strings.Contains(q, "a_bogus=sig-value") || !strings.Contains(q, "msToken=token-abc") {
		t.Fatalf("final query %q missing signer output", q)
	}
	if !strings.Contains(q, "device_platform=webapp") {
		t.Fatalf("final query %q lost original params", q)
	}
}

// TestStaticSignerNilFn: an empty StaticSigner is a no-op.
func TestStaticSignerNilFn(t *testing.T) {
	var sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{Timeout: 2 * time.Second, UserAgents: []string{"ua"}}).WithSigner(StaticSigner{})
	if _, _, err := c.Do(context.Background(), http.MethodGet, srv.URL+"?x=1", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sawQuery != "x=1" {
		t.Fatalf("query = %q, want unchanged", sawQuery)
	}
}

// TestDoContextTimeout: a slow server combined with a short ctx returns the
// context error without retrying.
func TestDoContextTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c := New(Config{MaxRetries: 1, Timeout: 2 * time.Second})
	_, _, err := c.Do(ctx, http.MethodGet, srv.URL, nil, nil)
	if err == nil {
		t.Fatal("Do succeeded despite ctx timeout")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Fatalf("err = %v, want context error", err)
	}
}

// TestDoBadScheme: only http/https are accepted.
func TestDoBadScheme(t *testing.T) {
	c := New(Config{})
	if _, _, err := c.Do(context.Background(), http.MethodGet, "ftp://host/x", nil, nil); err == nil {
		t.Fatal("ftp URL accepted, want error")
	}
	if _, _, err := c.Do(context.Background(), http.MethodGet, "://bad", nil, nil); err == nil {
		t.Fatal("malformed URL accepted, want error")
	}
}

// TestPropertyEchoRoundTrip: for any random request body the server echoes it
// back unchanged (seeded property).
func TestPropertyEchoRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf)
	}))
	defer srv.Close()

	c := New(Config{Timeout: 3 * time.Second, UserAgents: []string{"prop-ua"}})
	prop := testkit.Prop{
		Name: "echo_roundtrip",
		Inv: func(r *testkit.R) string {
			payload := r.Bytes(64 * 1024)
			status, body, err := c.Do(context.Background(), http.MethodPost, srv.URL, nil, payload)
			if err != nil {
				return "do: " + err.Error()
			}
			if status != http.StatusOK {
				return fmt.Sprintf("status %d", status)
			}
			if len(body) != len(payload) {
				return fmt.Sprintf("echo len %d != %d", len(body), len(payload))
			}
			for i := range payload {
				if body[i] != payload[i] {
					return fmt.Sprintf("echo diverged at byte %d", i)
				}
			}
			return ""
		},
	}
	testkit.Run(t, 20250101, 25, []testkit.Prop{prop})
}

// TestDoGzipAnswerToManualAcceptEncoding: a caller-declared Accept-Encoding
// (the browser header sets merge one under xhs/ks requests) makes Go's
// transport pass it verbatim WITHOUT transparent decompression — gzip-answering
// surfaces (xhs/ks corpus truth) then return Content-Encoding: gzip bytes.
// Do must decompress exactly that case so callers always see plain JSON.
func TestDoGzipAnswerToManualAcceptEncoding(t *testing.T) {
	const payload = `{"status_code":0,"comments":[{"cid":"c1"}]}`
	var sawOffer, sawEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawOffer = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(sawOffer, "gzip") {
			sawEncoding = "gzip"
			w.Header().Set("Content-Encoding", "gzip")
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write([]byte(payload))
			_ = zw.Close()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(buf.Bytes())
			return
		}
		sawEncoding = "identity"
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	c := New(Config{Timeout: 2 * time.Second})
	status, body, err := c.Do(context.Background(), http.MethodGet, srv.URL,
		map[string]string{"Accept-Encoding": "gzip, deflate"}, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK || string(body) != payload {
		t.Fatalf("gzip answer must come back decompressed: status=%d body=%q", status, body)
	}
	if sawEncoding != "gzip" {
		t.Fatalf("server did not answer gzip to offer %q (encoding=%s)", sawOffer, sawEncoding)
	}

	// Without the manual header the transport auto-negotiates and
	// auto-decompresses; the path stays plain either way.
	status, body, err = c.Do(context.Background(), http.MethodGet, srv.URL, nil, nil)
	if err != nil || status != http.StatusOK || string(body) != payload {
		t.Fatalf("plain path broken: %v status=%d body=%q", err, status, body)
	}
}
