package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// TestProbeDepthUsesRealCursor (report §3 defect / item 9): the depth check
// must fetch the REAL page-2 cursor returned by page 1. With the old
// hardcoded "20" the opaque-cursor platform treated it as an illegal cursor
// and answered empty, misjudging healthy accounts as expired.
func TestProbeDepthUsesRealCursor(t *testing.T) {
	var mu sync.Mutex
	var page2Cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := r.URL.Query().Get("cursor")
		w.Header().Set("Content-Type", "application/json")
		if cur == "" {
			// page 1: healthy data + the REAL next cursor (an opaque id)
			w.Write([]byte(`{"data":[{"id":"n1"},{"id":"n2"}],"has_more":true,"cursor":"opaque-id-7"}`))
			return
		}
		mu.Lock()
		page2Cursors = append(page2Cursors, cur)
		mu.Unlock()
		if cur == "opaque-id-7" {
			w.Write([]byte(`{"data":[{"id":"n3"}],"has_more":false,"cursor":"end"}`))
			return
		}
		// any other cursor (e.g. the hardcoded "20") is illegal → empty page
		w.Write([]byte(`{"data":[],"has_more":false}`))
	}))
	defer srv.Close()
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "probe-notes", Platform: "mock", Category: "user_posts", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/notes", Method: "GET"},
		Binding:   contracts.Binding{Items: "$.data"},
		Paging:    contracts.Paging{CursorParam: "cursor", HasMorePath: "$.has_more", NextCursorPath: "$.cursor"},
	})
	pool, err := accounts.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.Save(accounts.Account{ID: "p1", Platform: "mock", Cookies: map[string]string{"sess": "1"}})
	e := New(Context{
		Registry:  reg,
		HTTP:      httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}),
		Obs:       obs.NewCounterMap(),
		Names:     map[string]map[string]string{"mock": {"user_posts": "probe-notes"}},
		Accounts:  pool,
		AccountID: "p1",
	})
	pe := e.forAccount("p1")
	out, err := pe.ProbeAccount(context.Background(), "mock", "probe-notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(page2Cursors) != 1 || page2Cursors[0] != "opaque-id-7" {
		t.Fatalf("depth probe cursors = %v, want exactly the page-1 real cursor", page2Cursors)
	}
	if out.Health != accounts.HealthHealthy {
		t.Fatalf("health = %q (%s), want healthy with the real cursor", out.Health, out.Detail)
	}
}

// TestProbeDepthSkippedWhenNoMore: has_more=false on page 1 → no depth fetch
// at all (a single-page account must not be probed into a fake page 2).
func TestProbeDepthSkippedWhenNoMore(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"data":[{"id":"n1"}],"has_more":false,"cursor":""}`))
	}))
	defer srv.Close()
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "probe-one", Platform: "mock", Category: "user_posts", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/notes", Method: "GET"},
		Binding:   contracts.Binding{Items: "$.data"},
		Paging:    contracts.Paging{CursorParam: "cursor", HasMorePath: "$.has_more", NextCursorPath: "$.cursor"},
	})
	e := New(Context{Registry: reg, HTTP: httpclient.New(httpclient.Config{UserAgents: []string{"ua"}}), Obs: obs.NewCounterMap()})
	if _, err := e.ProbeAccount(context.Background(), "mock", "probe-one", nil); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("depth check fired %d requests, want 1", hits)
	}
}

// TestReplyTargetParamConfigurable (report t10 / item 6-④): a replies
// contract can declare transport.reply_target_param; the engine then keys
// the top-level comment id by that name. Default stays the first placeholder
// (TODO-C线: xhs 真名待 A 线结论，由 C 线在适配契约设置).
func TestReplyTargetParamConfigurable(t *testing.T) {
	var seenDefault, seenCustom string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Query().Get("root_comment_id") != "" {
			seenCustom = r.URL.Query().Get("root_comment_id")
		} else {
			seenDefault = r.URL.Query().Get("comment_id")
		}
		mu.Unlock()
		w.Write([]byte(`{"comments":[{"id":"r1"}],"has_more":false}`))
	}))
	defer srv.Close()
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "rep-default", Platform: "mock", Category: "replies", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/d", Method: "GET", Placeholders: []string{"comment_id"}},
		Binding:   contracts.Binding{Comments: "$.comments"},
	})
	reg.Add(&contracts.Contract{
		Name: "rep-custom", Platform: "mock", Category: "replies", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/c", Method: "GET", Placeholders: []string{"comment_id"}, ReplyTargetParam: "root_comment_id"},
		Binding:   contracts.Binding{Comments: "$.comments"},
	})
	e := New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}),
		Obs:      obs.NewCounterMap(),
		Names:    map[string]map[string]string{"mock": {"replies": "rep-default"}},
	})
	if _, _, err := e.CommentReplies(context.Background(), "mock", "it1", "cid-9", model.Cursor{}, 10); err != nil {
		t.Fatal(err)
	}
	e2 := New(Context{Registry: reg, HTTP: httpclient.New(httpclient.Config{UserAgents: []string{"ua"}}), Obs: obs.NewCounterMap(),
		Names: map[string]map[string]string{"mock": {"replies": "rep-custom"}}})
	if _, _, err := e2.CommentReplies(context.Background(), "mock", "it1", "cid-9", model.Cursor{}, 10); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenDefault != "cid-9" {
		t.Fatalf("default param name: comment_id=%q, want cid-9", seenDefault)
	}
	if seenCustom != "cid-9" {
		t.Fatalf("custom param name: root_comment_id=%q, want cid-9", seenCustom)
	}
}

// TestCursorSentinelExplicit (report item 11): the kuaishou "no_more" cursor
// stops the walk through an explicit sentinel check, not the numeric-string
// asBool accident.
func TestCursorSentinelExplicit(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Query().Get("pcursor") == "" {
			w.Write([]byte(`{"data":[{"id":"a"}],"has_more":true,"cursor":"no_more"}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"b"}],"has_more":true,"cursor":"no_more"}`))
	}))
	defer srv.Close()
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "sent-search", Platform: "mock", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/s", Method: "GET"},
		Binding:   contracts.Binding{Items: "$.data"},
		Paging:    contracts.Paging{CursorParam: "pcursor", HasMorePath: "$.has_more", NextCursorPath: "$.cursor"},
	})
	e := New(Context{Registry: reg, HTTP: httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}), Obs: obs.NewCounterMap(),
		Names: map[string]map[string]string{"mock": {"search": "sent-search"}}})
	items, cur, err := e.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || hits != 1 {
		t.Fatalf("sentinel did not stop the walk: items=%d hits=%d", len(items), hits)
	}
	if cur.HasMore {
		t.Fatal("has_more must be false after a sentinel cursor")
	}
	if !strings.Contains(asStr(cur.Source["cursor"]), "no_more") {
		t.Fatalf("cursor value should be preserved for resume bookkeeping: %+v", cur.Source)
	}
}
