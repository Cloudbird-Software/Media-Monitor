package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// upsServer: a two-page douyin-user-posts fixture platform that records the
// query of every request so parameter passthrough is assertable per field.
type upsServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	qrys []map[string]string
}

func newUpsServer(t *testing.T) *upsServer {
	t.Helper()
	ps := &upsServer{}
	page := func(ids []string, digs []int64, next string, more bool) string {
		var b strings.Builder
		b.WriteString(`{"aweme_list":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"aweme_id":%q,"desc":"d","create_time":%d,"type":1,"statistics":{"digg_count":%d,"comment_count":1,"collect_count":1,"share_count":1},"author":{"sec_uid":"s","nickname":"n"}}`, id, 1780500000-i*100, digs[i])
		}
		fmt.Fprintf(&b, `],"has_more":%v,"max_cursor":%q}`, more, next)
		return b.String()
	}
	ps.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := map[string]string{}
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				q[k] = v[0]
			}
		}
		ps.mu.Lock()
		ps.qrys = append(ps.qrys, q)
		ps.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("max_cursor") == "" {
			fmt.Fprint(w, page([]string{"i1", "i2"}, []int64{5, 5}, "c2", true))
			return
		}
		fmt.Fprint(w, page([]string{"i3", "i4", "i5"}, []int64{5, 90000, 90000}, "", false))
	}))
	return ps
}

func upsAdaptDir(t *testing.T, srvURL string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "contracts"), 0o755); err != nil {
		t.Fatal(err)
	}
	contract := fmt.Sprintf(`{
	  "name": "douyin-user-posts", "platform": "douyin", "category": "user_posts", "version": "1",
	  "transport": {"base_url": %q, "path": "/post/", "method": "GET", "placeholders": ["sec_user_id"]},
	  "binding": {"items": "$.aweme_list"},
	  "paging": {"cursor_param": "max_cursor", "count_param": "count", "count_default": 20, "has_more_path": "$.has_more", "next_cursor_path": "$.max_cursor"}
	}`, srvURL)
	if err := os.WriteFile(filepath.Join(dir, "contracts", "douyin-user-posts.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestUserPostsToolRegisteredAndSchema: tools/list carries get_user_posts
// with the full parameter surface including the nested min_engagement
// object and the metric enum (W3-C2 AC-1 / IF-1 zero-shot schema).
func TestUserPostsToolRegisteredAndSchema(t *testing.T) {
	t.Setenv("MEDIAMON_ADAPT_DIR", writeAdaptDir(t))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c := startServer(t)
	m := c.call(t, "tools/list", "l1", nil)
	tools := m["result"].(map[string]any)["tools"].([]any)
	for _, tl := range tools {
		obj := tl.(map[string]any)
		if obj["name"] != "get_user_posts" {
			continue
		}
		schema := obj["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		for _, p := range []string{"platform", "sec_uid", "window_months", "min_engagement", "stop_after_consecutive", "limit", "cursor", "account_id"} {
			if _, ok := props[p]; !ok {
				t.Fatalf("get_user_posts schema missing %q", p)
			}
		}
		me := props["min_engagement"].(map[string]any)
		mep := me["properties"].(map[string]any)
		enum := mep["metric"].(map[string]any)["enum"].([]any)
		want := []string{"digg", "comment", "share", "collect", "play"}
		if len(enum) != len(want) {
			t.Fatalf("metric enum = %v", enum)
		}
		return
	}
	t.Fatal("get_user_posts not registered")
}

// TestUserPostsToolBacktrackPassthrough: window/engagement/N/cursor all
// reach the engine — the fixture walk stops after exactly 3 consecutive
// low items and the returned cursor resumes from the stopping page.
func TestUserPostsToolBacktrackPassthrough(t *testing.T) {
	ps := newUpsServer(t)
	defer ps.srv.Close()
	t.Setenv("MEDIAMON_ADAPT_DIR", upsAdaptDir(t, ps.srv.URL))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c := startServer(t)

	out := c.callTool(t, "get_user_posts", map[string]any{
		"platform": "douyin", "sec_uid": "sec-1",
		"min_engagement":         map[string]any{"metric": "digg", "threshold": 1000},
		"stop_after_consecutive": 3,
		"window_months":          6,
		"limit":                  20,
	})
	if out == nil {
		t.Fatal("get_user_posts failed")
	}
	res := out
	items := res["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (stops at the 3rd consecutive low)", len(items))
	}
	next, ok := res["next_cursor"].(map[string]any)
	if !ok || next["v"] != float64(1) {
		t.Fatalf("next_cursor = %v", res["next_cursor"])
	}
	ps.mu.Lock()
	qrys := append([]map[string]string(nil), ps.qrys...)
	ps.mu.Unlock()
	if len(qrys) != 2 {
		t.Fatalf("pages = %d, want 2 (stop after 3rd low on page 2)", len(qrys))
	}
	if qrys[0]["sec_user_id"] != "sec-1" {
		t.Fatalf("sec_user_id passthrough = %v", qrys[0]["sec_user_id"])
	}
}

// TestUserPostsToolFailClosedPlatform: kuaishou declares no user_posts
// contract — the tool surfaces the engine's explicit not-declared error.
// TestUserPostsToolKuaishouLeg: capability batch 1 declared the ks user_posts
// face (kuaishou-profile-feed), so the old "kuaishou not-declared" fail-closed
// premise is stale. The ks leg now gets positive coverage: the profile/feed
// shape (POST body userId/pcursor, photo-carrying feeds, "no_more" sentinel)
// must bind through the tool. The undeclared fail-closed extreme itself stays
// covered by cmd/mediactl's fc-up-shipinhao-not-declared matrix row.
func TestUserPostsToolKuaishouLeg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(body, &b)
		w.Header().Set("Content-Type", "application/json")
		if b["pcursor"] == "no_more" || (fmt.Sprint(b["userId"]) == "") {
			fmt.Fprint(w, `{"result":1,"pcursor":"no_more","feeds":[]}`)
			return
		}
		fmt.Fprint(w, `{"result":1,"pcursor":"no_more","feeds":[
			{"type":1,"tags":[],"author":{"id":"3xk","name":"快手作者","headerUrl":"http://h/1"},
			 "photo":{"id":"p1","timestamp":1780000000000,"like_count":42},"comment":{"us_c":0}},
			{"type":1,"tags":[],"author":{"id":"3xk","name":"快手作者","headerUrl":"http://h/1"},
			 "photo":{"id":"p2","timestamp":1779999000000,"like_count":7},"comment":{"us_c":0}}]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cdir := filepath.Join(dir, "contracts")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	contract := fmt.Sprintf(`{
	  "name": "kuaishou-profile-feed", "platform": "kuaishou", "category": "user_posts", "version": "1",
	  "transport": {"base_url": %q, "path": "/rest/v/profile/feed", "method": "POST", "body": {"kpn": "PC_WEB"}, "placeholders": ["userId"]},
	  "binding": {"items": "$.feeds",
	    "fields": {"id": "$.feeds[].photo.id", "create_time": "$.feeds[].photo.timestamp", "stats.digg": "$.feeds[].photo.like_count", "author.avatar_url": "$.feeds[].author.headerUrl"}},
	  "paging": {"cursor_param": "pcursor", "has_more_path": "$.pcursor", "next_cursor_path": "$.pcursor"}
	}`, srv.URL)
	if err := os.WriteFile(filepath.Join(cdir, "kuaishou-profile-feed.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEDIAMON_ADAPT_DIR", dir)
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	t.Setenv("MEDIAMON_KUAISHOU_COOKIES", "did=test-did")
	c := startServer(t)
	res := c.callTool(t, "get_user_posts", map[string]any{"platform": "kuaishou", "sec_uid": "3xk"})
	items, ok := res["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("ks user posts items = %v", res["items"])
	}
	first := items[0].(map[string]any)
	if first["id"] != "p1" {
		t.Fatalf("ks item id = %v (photo.id binding)", first["id"])
	}
}

// TestUserPostsToolCursorResume: feeding next_cursor back continues the
// walk from the stopping position without re-serving earlier items.
func TestUserPostsToolCursorResume(t *testing.T) {
	ps := newUpsServer(t)
	defer ps.srv.Close()
	t.Setenv("MEDIAMON_ADAPT_DIR", upsAdaptDir(t, ps.srv.URL))
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	c := startServer(t)

	first := c.callTool(t, "get_user_posts", map[string]any{
		"platform": "douyin", "sec_uid": "sec-1", "limit": 2,
	})
	res := first
	next := res["next_cursor"].(map[string]any)
	second := c.callTool(t, "get_user_posts", map[string]any{
		"platform": "douyin", "sec_uid": "sec-1", "limit": 2, "cursor": next,
	})
	items2 := second["items"].([]any)
	if len(items2) == 0 {
		t.Fatal("resume returned no items")
	}
	firstID := fmt.Sprint(res["items"].([]any)[0].(map[string]any)["id"])
	secondID := fmt.Sprint(items2[0].(map[string]any)["id"])
	if firstID == secondID {
		t.Fatalf("resume re-served page 1 (first id %s)", firstID)
	}
}
