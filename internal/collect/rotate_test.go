package collect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

// rotEnv: a search contract whose endpoint answers per the carried cookie —
// acc1 gets 401, anyone else gets data. Requests are recorded as
// "cookie|cursor" so rotate-with-same-cursor is assertable on the wire.
type rotEnv struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string
	eng  *Engine
	pool *accounts.Pool
}

func newRotEnv(t *testing.T) *rotEnv {
	t.Helper()
	env := &rotEnv{}
	env.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		cur := r.URL.Query().Get("cursor")
		env.mu.Lock()
		env.reqs = append(env.reqs, cookie+"|"+cur)
		env.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(cookie, "acc1") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if cur == "" {
			fmt.Fprint(w, `{"data":[{"id":"p1-a","desc":"x"}],"has_more":true,"cursor":"c2"}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"p2-b","desc":"y"}],"has_more":false,"cursor":""}`)
	}))
	dir := t.TempDir()
	contract := fmt.Sprintf(`{
	  "name": "douyin-search", "platform": "douyin", "category": "search", "version": "1",
	  "transport": {"base_url": %q, "path": "/s/", "method": "GET", "placeholders": ["keyword"]},
	  "binding": {"items": "$.data"},
	  "paging": {"cursor_param": "cursor", "count_param": "count", "count_default": 20, "has_more_path": "$.has_more", "next_cursor_path": "$.cursor"}
	}`, env.srv.URL)
	if err := os.WriteFile(filepath.Join(dir, "douyin-search.json"), []byte(contract), 0o644); err != nil {
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
	for _, id := range []string{"acc1", "acc2"} {
		if err := pool.Save(accounts.Account{ID: id, Platform: "douyin", Cookies: map[string]string{"sess": id}}); err != nil {
			t.Fatal(err)
		}
	}
	env.pool = pool
	env.eng = New(Context{
		Registry:  reg,
		HTTP:      httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}),
		Obs:       obs.NewCounterMap(),
		Names:     map[string]map[string]string{"douyin": {"search": "douyin-search"}},
		Accounts:  pool,
		AccountID: autoAccountID,
	})
	return env
}

// TestAutoRotationFromAuthWall: acc1 (healthy pick) answers 401; the walk
// rotates to acc2, retries the SAME page (cursor "" again — the walk never
// restarts) and completes both pages; acc1 stays active (single failure).
func TestAutoRotationFromAuthWall(t *testing.T) {
	env := newRotEnv(t)
	defer env.srv.Close()
	if err := env.pool.SetHealth("acc1", accounts.HealthHealthy, ""); err != nil {
		t.Fatal(err)
	}
	if err := env.pool.SetHealth("acc2", accounts.HealthDegraded, "seed"); err != nil {
		t.Fatal(err)
	}

	items, cur, err := env.eng.SearchItems(context.Background(), "douyin", "k", "", model.Cursor{}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "p1-a" || items[1].ID != "p2-b" {
		t.Fatalf("items = %+v", items)
	}
	if cur.HasMore {
		t.Fatal("terminal page must end has_more=false")
	}
	env.mu.Lock()
	reqs := append([]string(nil), env.reqs...)
	env.mu.Unlock()
	want := []string{"sess=acc1|", "sess=acc2|", "sess=acc2|c2"}
	if len(reqs) != len(want) {
		t.Fatalf("requests = %v, want %v", reqs, want)
	}
	for i := range want {
		if reqs[i] != want[i] {
			t.Fatalf("request[%d] = %q, want %q", i, reqs[i], want[i])
		}
	}
	a1, _ := env.pool.Get("acc1")
	if a1.Status != accounts.StatusActive {
		t.Fatalf("acc1 status = %q, want active after one failure", a1.Status)
	}
}

// newAllFailEnv: every account answers 401.
func newAllFailEnv(t *testing.T) (*Engine, *accounts.Pool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	contract := fmt.Sprintf(`{"name":"douyin-search","platform":"douyin","category":"search","version":"1","transport":{"base_url":%q,"path":"/s/","method":"GET","placeholders":["keyword"]},"binding":{"items":"$.data"}}`, srv.URL)
	if err := os.WriteFile(filepath.Join(dir, "douyin-search.json"), []byte(contract), 0o644); err != nil {
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
	eng := New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second}),
		Obs:      obs.NewCounterMap(),
		Names:    map[string]map[string]string{"douyin": {"search": "douyin-search"}},
		Accounts: pool, AccountID: autoAccountID,
	})
	return eng, pool
}

// TestAutoRotationBounded: every account fails — bounded rotations, then an
// explicit pool-exhausted error (no hang, no silent partial data).
func TestAutoRotationBounded(t *testing.T) {
	eng, pool := newAllFailEnv(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := pool.Save(accounts.Account{ID: id, Platform: "douyin", Cookies: map[string]string{"sess": id}}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := eng.SearchItems(context.Background(), "douyin", "k", "", model.Cursor{}, 20)
	if err == nil {
		t.Fatal("want explicit error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no usable account") && !strings.Contains(msg, "rotation exhausted") {
		t.Fatalf("err = %v, want bounded/exhausted explicit error", err)
	}
}

// TestAutoBanAfterThreeFailures: three consecutive failing walks under one
// usable account ban it (persisted); the next walk fails closed.
func TestAutoBanAfterThreeFailures(t *testing.T) {
	eng, pool := newAllFailEnv(t)
	if err := pool.Save(accounts.Account{ID: "solo", Platform: "douyin", Cookies: map[string]string{"sess": "solo"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := eng.SearchItems(context.Background(), "douyin", "k", "", model.Cursor{}, 20); err == nil {
			t.Fatalf("walk %d: expected auth-wall failure", i)
		}
	}
	a, _ := pool.Get("solo")
	if a.Status != accounts.StatusBanned {
		t.Fatalf("status = %q, want banned after 3 consecutive failures", a.Status)
	}
	if _, _, err := eng.SearchItems(context.Background(), "douyin", "k", "", model.Cursor{}, 20); err == nil || !strings.Contains(err.Error(), "no usable account") {
		t.Fatalf("err = %v, want pool-empty fail-closed", err)
	}
}

// TestPickForHealthRanking: healthy > degraded > unprobed; banned and other
// platforms never picked; the exclude set always wins (W4-C2 AC-1).
func TestPickForHealthRanking(t *testing.T) {
	pool, err := accounts.Open(filepath.Join(t.TempDir(), "pool"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, a := range []accounts.Account{
		{ID: "deg", Platform: "douyin"},
		{ID: "heal", Platform: "douyin"},
		{ID: "unp", Platform: "douyin"},
		{ID: "ban", Platform: "douyin", Status: accounts.StatusBanned},
		{ID: "other", Platform: "xhs"},
	} {
		if err := pool.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.SetHealth("deg", accounts.HealthDegraded, "s"); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetHealth("heal", accounts.HealthHealthy, "s"); err != nil {
		t.Fatal(err)
	}
	if a, ok := pool.PickFor("douyin", nil); !ok || a.ID != "heal" {
		t.Fatalf("pick = %+v ok=%v, want heal", a, ok)
	}
	if a, ok := pool.PickFor("douyin", map[string]bool{"heal": true}); !ok || a.ID != "deg" {
		t.Fatalf("pick excl heal = %+v, want deg", a)
	}
	if a, ok := pool.PickFor("douyin", map[string]bool{"heal": true, "deg": true}); !ok || a.ID != "unp" {
		t.Fatalf("pick excl heal,deg = %+v, want unp", a)
	}
	if _, ok := pool.PickFor("douyin", map[string]bool{"heal": true, "deg": true, "unp": true}); ok {
		t.Fatal("banned/other-platform must never be picked")
	}
}

// TestAutoEmptyPoolFailClosed: auto mode with no usable account fails with
// an explicit error before any request (W4-C2 AC-6).
func TestAutoEmptyPoolFailClosed(t *testing.T) {
	eng, _ := newAllFailEnv(t) // pool open but empty
	_, _, err := eng.SearchItems(context.Background(), "douyin", "k", "", model.Cursor{}, 20)
	if err == nil || !strings.Contains(err.Error(), "no usable account") {
		t.Fatalf("err = %v, want explicit pool-empty error", err)
	}
}
