package collect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

// probeFixture is a mock probe target: page 1 with cursor absent, page 2
// keyed by the depth cursor. mode selects the response shape.
type probeFixture struct {
	srv  *httptest.Server
	mu   sync.Mutex
	mode string // ok | empty1 | empty2 | auth1 | auth2 | fail2
	// last request observations (prove the account context rode the probe)
	lastCookie string
	lastUA     string
	lastCursor string
}

func newProbeFixture(t *testing.T, mode string) *probeFixture {
	t.Helper()
	pf := &probeFixture{mode: mode}
	pf.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pf.mu.Lock()
		pf.lastCookie = r.Header.Get("Cookie")
		pf.lastUA = r.Header.Get("User-Agent")
		pf.lastCursor = r.URL.Query().Get("cursor")
		mode := pf.mode
		pf.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case mode == "auth1":
			w.WriteHeader(http.StatusUnauthorized)
			return
		case mode == "auth2" && r.URL.Query().Get("cursor") != "":
			w.WriteHeader(http.StatusUnauthorized)
			return
		case mode == "empty1" && r.URL.Query().Get("cursor") == "":
			fmt.Fprint(w, `{"notes": [], "has_more": false, "cursor": ""}`)
			return
		case mode == "empty2" && r.URL.Query().Get("cursor") != "":
			fmt.Fprint(w, `{"notes": [], "has_more": false, "cursor": ""}`)
			return
		case mode == "fail2" && r.URL.Query().Get("cursor") != "":
			w.WriteHeader(http.StatusBadGateway)
			return
		default:
			if r.URL.Query().Get("cursor") == "" {
				fmt.Fprint(w, `{"notes": [{"note_id": "n1"}, {"note_id": "n2"}], "has_more": true, "cursor": "c2"}`)
				return
			}
			fmt.Fprint(w, `{"notes": [{"note_id": "n3"}], "has_more": false, "cursor": ""}`)
		}
	}))
	return pf
}

// probeSetup wires an engine bound to a pool account against pf.
func probeSetup(t *testing.T, pf *probeFixture) (*Engine, *accounts.Pool) {
	t.Helper()
	dir := t.TempDir()
	contract := fmt.Sprintf(`{
	  "name": "xhs-user-notes", "platform": "xhs", "category": "user_posts", "version": "1",
	  "transport": {"base_url": %q, "path": "/posted", "method": "GET", "placeholders": ["user_id"]},
	  "cookie": {"required": ["web_session"]},
	  "binding": {"items": "$.notes"},
	  "paging": {"cursor_param": "cursor", "count_param": "num", "count_default": 30, "has_more_path": "$.has_more", "next_cursor_path": "$.cursor"}
	}`, pf.srv.URL)
	if err := os.WriteFile(filepath.Join(dir, "xhs-user-notes.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, dir); err != nil {
		t.Fatal(err)
	}
	pool, err := accounts.Open(filepath.Join(t.TempDir(), "pool"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	acct := accounts.Account{
		ID: "acc-1", Platform: "xhs",
		Cookies: map[string]string{"web_session": "sess-acc1"},
		UA:      "UA-ACC1/1.0",
	}
	if err := pool.Save(acct); err != nil {
		t.Fatal(err)
	}
	eng := New(Context{
		Registry:  reg,
		HTTP:      httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"pool-ua"}}),
		Obs:       obs.NewCounterMap(),
		Accounts:  pool,
		AccountID: "acc-1",
	})
	return eng, pool
}

// TestProbeAccountMatrix: the four canonical platform behaviors classify to
// the right health form through the full engine walk (W4-C1 AC-2).
func TestProbeAccountMatrix(t *testing.T) {
	cases := []struct {
		mode string
		want accounts.Health
	}{
		{"ok", accounts.HealthHealthy},
		{"auth1", accounts.HealthExpired},
		{"empty1", accounts.HealthExpired},
		{"empty2", accounts.HealthExpired},
		{"auth2", accounts.HealthExpired},
		{"fail2", accounts.HealthDegraded},
	}
	for _, c := range cases {
		pf := newProbeFixture(t, c.mode)
		eng, pool := probeSetup(t, pf)
		out, err := eng.ProbeAndStore(context.Background(), pool, "acc-1", "xhs-user-notes", map[string]string{"user_id": "u1"})
		if err != nil {
			t.Fatalf("%s: ProbeAndStore: %v", c.mode, err)
		}
		if out.Health != c.want {
			t.Fatalf("%s: health = %q (%s), want %q", c.mode, out.Health, out.Detail, c.want)
		}
		// persisted (AC-1)
		a, _ := pool.Get("acc-1")
		if a.Health != c.want || a.HealthCheckedAt == 0 {
			t.Fatalf("%s: health not persisted on pool: %+v", c.mode, a)
		}
		pf.srv.Close()
		pool.Close()
	}
}

// TestProbeRidesAccountContext: every probe request carries the account's
// own cookie and UA, not the platform defaults (W4-C1 AC-5).
func TestProbeRidesAccountContext(t *testing.T) {
	pf := newProbeFixture(t, "ok")
	eng, pool := probeSetup(t, pf)
	if _, err := eng.ProbeAndStore(context.Background(), pool, "acc-1", "xhs-user-notes", map[string]string{"user_id": "u1"}); err != nil {
		t.Fatal(err)
	}
	pf.mu.Lock()
	cookie, ua := pf.lastCookie, pf.lastUA
	pf.mu.Unlock()
	if cookie != "web_session=sess-acc1" {
		t.Fatalf("probe cookie = %q, want the account's own web_session", cookie)
	}
	if ua != "UA-ACC1/1.0" {
		t.Fatalf("probe UA = %q, want the account's pinned UA", ua)
	}
}

// TestProbeRequiresContractParams: a contract placeholder without a value
// fails closed with an explicit error (no half-probe).
func TestProbeRequiresContractParams(t *testing.T) {
	pf := newProbeFixture(t, "ok")
	eng, _ := probeSetup(t, pf)
	if _, err := eng.ProbeAccount(context.Background(), "xhs", "xhs-user-notes", map[string]string{}); err == nil {
		t.Fatal("missing user_id must fail closed")
	}
}

// TestProbeUnknownPlatformDefault: kuaishou has no declared probe contract —
// explicit error, never a silent pass.
func TestProbeUnknownPlatformDefault(t *testing.T) {
	if _, _, err := DefaultProbeContract("kuaishou"); err == nil {
		t.Fatal("kuaishou must fail closed (no probe contract declared)")
	}
}

// TestProbeDefaultContractHonorsCallerParams: the default-contract path must
// overlay caller-supplied params on the platform defaults instead of
// dropping them (B1 — the previous reassignment discarded the kv map, so
// the documented `--param user_id=...` usage could never reach the
// placeholder check and every default-path probe failed closed).
func TestProbeDefaultContractHonorsCallerParams(t *testing.T) {
	pf := newProbeFixture(t, "ok")
	eng, pool := probeSetup(t, pf)
	defer pf.srv.Close()
	defer pool.Close()
	out, err := eng.ProbeAndStore(context.Background(), pool, "acc-1", "", map[string]string{"user_id": "u1"})
	if err != nil {
		t.Fatalf("default-contract probe with caller params: %v", err)
	}
	if out.Health != accounts.HealthHealthy {
		t.Fatalf("health = %q (%s), want healthy", out.Health, out.Detail)
	}
}
