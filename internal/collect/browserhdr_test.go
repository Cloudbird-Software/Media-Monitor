package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

// browserTestServer records the headers of each request and answers one page.
func browserTestServer(t *testing.T, hdrs *[]http.Header, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*hdrs = append(*hdrs, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"x"}],"has_more":false}`))
	}))
}

func browserEngine(t *testing.T, srvURL string, contractHeaders map[string]string, mutate func(*Context)) *Engine {
	t.Helper()
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "hdr-search", Platform: "mock", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srvURL, Path: "/list", Method: "GET", Headers: contractHeaders},
		Binding:   contracts.Binding{Items: "$.data"},
	})
	ctx := Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Obs:      obs.NewCounterMap(),
		Names:    map[string]map[string]string{"mock": {"search": "hdr-search"}},
		BrowserHeaders: map[string]map[string]string{
			"mock": {
				"Accept":          "application/json, text/plain, */*",
				"Accept-Language": "zh-CN,zh;q=0.9",
				"Referer":         "https://mock.example.com/",
				"Sec-Fetch-Dest":  "empty",
				"Sec-Fetch-Mode":  "cors",
				"Sec-Fetch-Site":  "same-origin",
				"Priority":        "u=1, i",
			},
		},
	}
	if mutate != nil {
		mutate(&ctx)
	}
	return New(ctx)
}

// TestBrowserHeadersMerged: a platform with registered browser defaults
// sends the full browser-grade set (report B1: baseline is 19-28 headers;
// before this change the collector sent 4-5).
func TestBrowserHeadersMerged(t *testing.T) {
	var hdrs []http.Header
	var mu sync.Mutex
	srv := browserTestServer(t, &hdrs, &mu)
	defer srv.Close()
	e := browserEngine(t, srv.URL, nil, nil)
	if _, _, err := e.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 5); err != nil {
		t.Fatal(err)
	}
	if len(hdrs) != 1 {
		t.Fatalf("requests = %d", len(hdrs))
	}
	h := hdrs[0]
	for _, k := range []string{"Accept", "Accept-Language", "Referer", "Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Fetch-Site"} {
		if h.Get(k) == "" {
			t.Errorf("browser header %q missing (had %d headers total)", k, len(h))
		}
	}
	// The ≥15-header acceptance bar (task verification) is measured on the
	// full three-platform posture (browser set + UA + sec-ch-ua family +
	// cookie) in the mock-chain validation; here we assert the merged set
	// itself: 7 browser defaults + UA + transport Accept-Encoding ≥ 9.
	if n := len(h); n < 9 {
		t.Errorf("header count = %d, want >= 9 (browser defaults + UA + enc)", n)
	}
}

// TestContractHeadersOverrideBrowser: contract transport.headers win over the
// platform browser defaults (deploy-specific customization stays possible).
func TestContractHeadersOverrideBrowser(t *testing.T) {
	var hdrs []http.Header
	var mu sync.Mutex
	srv := browserTestServer(t, &hdrs, &mu)
	defer srv.Close()
	e := browserEngine(t, srv.URL, map[string]string{"Referer": "https://custom.example.com/x"}, nil)
	if _, _, err := e.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 5); err != nil {
		t.Fatal(err)
	}
	if got := hdrs[0].Get("Referer"); got != "https://custom.example.com/x" {
		t.Fatalf("contract header must override browser default: %q", got)
	}
	if hdrs[0].Get("Accept-Language") == "" {
		t.Fatal("non-overridden browser defaults must survive")
	}
}

// TestNoBrowserHeadersLegacy: an engine without BrowserHeaders behaves
// exactly as before (no referer injected).
func TestNoBrowserHeadersLegacy(t *testing.T) {
	var hdrs []http.Header
	var mu sync.Mutex
	srv := browserTestServer(t, &hdrs, &mu)
	defer srv.Close()
	e := browserEngine(t, srv.URL, nil, func(c *Context) { c.BrowserHeaders = nil })
	if _, _, err := e.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 5); err != nil {
		t.Fatal(err)
	}
	if hdrs[0].Get("Referer") != "" || hdrs[0].Get("Accept-Language") != "" {
		t.Fatal("legacy engine must not send browser defaults")
	}
}

// TestCookieJarSessionBinding: Set-Cookie from the server persists within the
// session (cookie lifetime) and is replayed on the next request; explicit
// account cookies still ride along; distinct accounts keep distinct jars.
func TestCookieJarSessionBinding(t *testing.T) {
	var mu sync.Mutex
	var reqs []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Header.Clone())
		mu.Unlock()
		w.Header().Add("Set-Cookie", "msToken=rot1; Path=/")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"x"}],"has_more":false}`))
	}))
	defer srv.Close()

	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "jar-search", Platform: "mock", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/list", Method: "GET"},
		Binding:   contracts.Binding{Items: "$.data"},
	})
	base := func() Context {
		return Context{
			Registry: reg,
			HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}),
			Obs:      obs.NewCounterMap(),
			Names:    map[string]map[string]string{"mock": {"search": "jar-search"}},
		}
	}

	// Same engine (same platform-default session): the jar replays msToken.
	e := New(base())
	for i := 0; i < 2; i++ {
		if _, _, err := e.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 5); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if got := reqs[0].Get("Cookie"); got != "" {
		t.Fatalf("first request must carry no jar cookie yet, got %q", got)
	}
	if got := reqs[1].Get("Cookie"); got != "msToken=rot1" {
		t.Fatalf("second request must replay the set-cookie (jar binding), got %q", got)
	}
}

// TestCookieJarPerAccount: distinct accounts get distinct jars — a rotation
// to another id never inherits the first id's msToken, and the first id's
// jar survives the switch (shared session cache, per-id keys).
func TestCookieJarPerAccount(t *testing.T) {
	var mu sync.Mutex
	var log []struct{ who, cookie string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		log = append(log, struct{ who, cookie string }{r.URL.Query().Get("who"), r.Header.Get("Cookie")})
		mu.Unlock()
		w.Header().Add("Set-Cookie", "msToken="+r.URL.Query().Get("who")+"; Path=/")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"x"}],"has_more":false}`))
	}))
	defer srv.Close()

	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "jar2-search", Platform: "mock", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/list", Method: "GET"},
		Binding:   contracts.Binding{Items: "$.data"},
	})
	ctx := Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}),
		Obs:      obs.NewCounterMap(),
		Names:    map[string]map[string]string{"mock": {"search": "jar2-search"}},
	}
	// Three engines sharing one session cache — the rotation shape produced
	// by forAccount clones.
	eA1, eA2, eB := New(ctx), New(ctx), New(ctx)
	eA2.sess, eB.sess = eA1.sess, eA1.sess
	eA1.accountID, eA2.accountID, eB.accountID = "acc-a", "acc-a", "acc-b"
	run := func(e *Engine, who string) {
		if _, err := e.Fetch(context.Background(), "jar2-search", nil, map[string]string{"who": who}); err != nil {
			t.Fatal(err)
		}
	}
	run(eA1, "a") // session A: empty cookie, server sets msToken=a
	run(eA2, "a") // same session key (acc-a): replays msToken=a
	run(eB, "b")  // session B: must NOT inherit msToken=a
	run(eA2, "a") // session A intact after B ran

	mu.Lock()
	defer mu.Unlock()
	if len(log) != 4 {
		t.Fatalf("requests = %d", len(log))
	}
	if log[0].cookie != "" {
		t.Fatalf("first request: no jar cookie yet, got %q", log[0].cookie)
	}
	if log[1].cookie != "msToken=a" {
		t.Fatalf("same-session request must replay jar cookie, got %q", log[1].cookie)
	}
	if log[2].cookie != "" {
		t.Fatalf("other account must have a separate jar, got %q", log[2].cookie)
	}
	if log[3].cookie != "msToken=a" {
		t.Fatalf("session A must survive the account switch, got %q", log[3].cookie)
	}
}
