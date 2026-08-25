package xhs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

func contractsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..", "..", "adapt", "contracts")
}

func remapContracts(t *testing.T, dir string, srv *httptest.Server, names ...string) *contracts.Registry {
	t.Helper()
	all := contracts.NewRegistry()
	if err := contracts.LoadDir(all, dir); err != nil {
		t.Fatal(err)
	}
	reg := contracts.NewRegistry()
	for _, n := range names {
		c, ok := all.Get(n)
		if !ok {
			t.Fatalf("contract %q not found in %s", n, dir)
		}
		cp := *c
		cp.Transport.BaseURL = srv.URL
		if err := reg.Add(&cp); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func mockEngine(t *testing.T, reg *contracts.Registry, names map[string]map[string]string) *collect.Engine {
	t.Helper()
	return collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Obs:      obs.NewCounterMap(),
		Names:    names,
	})
}

// TestXhsCommentsFlow: the real xhs-comments contract binds
// data.comments records (id/content/like_count, user_info author object)
// with default field paths; single page when has_more is false.
func TestXhsCommentsFlow(t *testing.T) {
	var sawNoteID, sawCursor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sawNoteID = q.Get("note_id")
		sawCursor = q.Get("cursor")
		_, _ = w.Write([]byte(`{
			"data": {
				"comments": [
					{"id": "xhs-comment-0001", "content": "小红书示例评论一", "create_time": 1780005001, "like_count": "30",
					 "user_info": {"user_id": "xhs-user-0003", "nickname": "小红书评论者一", "avatar": "https://example.invalid/xhsc-1.jpg", "ip_location": "广东"}},
					{"id": "xhs-comment-0002", "content": "小红书示例评论二", "create_time": 1780005002, "like_count": "5",
					 "user_info": {"user_id": "xhs-user-0004", "nickname": "小红书评论者二", "avatar": "https://example.invalid/xhsc-2.jpg", "ip_location": "四川"}}
				],
				"has_more": false,
				"cursor": ""
			}
		}`))
	}))
	defer srv.Close()

	dir := contractsDir(t)
	d, _, _ := Defaults(dir)
	reg := remapContracts(t, dir, srv, "xhs-comments")
	eng := mockEngine(t, reg, map[string]map[string]string{Platform: d.Names})

	cmts, cur, err := eng.ItemComments(context.Background(), Platform, "example-note-0001", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("ItemComments: %v", err)
	}
	if len(cmts) != 2 {
		t.Fatalf("comments = %d, want 2", len(cmts))
	}
	c0 := cmts[0]
	if c0.CID != "xhs-comment-0001" || c0.Text != "小红书示例评论一" || c0.CreateTime != 1780005001 || c0.DiggCount != 30 {
		t.Fatalf("comment fields wrong: %+v", c0)
	}
	if c0.User.UID != "xhs-user-0003" || c0.User.Nickname != "小红书评论者一" || c0.User.IPLabel != "广东" || c0.User.AvatarURL != "https://example.invalid/xhsc-1.jpg" {
		t.Fatalf("author wrong: %+v", c0.User)
	}
	if cur.HasMore {
		t.Fatalf("cursor = %+v, want single page", cur)
	}
	if sawNoteID != "example-note-0001" {
		t.Fatalf("note_id = %q", sawNoteID)
	}
	if sawCursor != "" {
		t.Fatalf("cursor param on first page = %q, want empty", sawCursor)
	}
}

// TestXhsSearchFlow: the real xhs-search contract binds note records
// (id/note_card fields) and classifies media type from note_card.type.
func TestXhsSearchFlow(t *testing.T) {
	var sawKeyword string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawKeyword = r.URL.Query().Get("keyword")
		_, _ = w.Write([]byte(`{
			"data": {
				"items": [
					{"id": "example-note-0001", "model_type": "note",
					 "note_card": {"display_title": "小红书示例笔记一", "type": "video", "user": {"user_id": "xhs-user-0001", "nickname": "小红书示例作者一"}}},
					{"id": "example-note-0002", "model_type": "note",
					 "note_card": {"display_title": "小红书示例笔记二", "type": "normal", "user": {"user_id": "xhs-user-0002", "nickname": "小红书示例作者二"}}}
				],
				"has_more": false,
				"cursor": ""
			}
		}`))
	}))
	defer srv.Close()

	dir := contractsDir(t)
	d, _, _ := Defaults(dir)
	reg := remapContracts(t, dir, srv, "xhs-search")
	eng := mockEngine(t, reg, map[string]map[string]string{Platform: d.Names})

	items, _, err := eng.SearchItems(context.Background(), Platform, "露营", "", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].ID != "example-note-0001" || items[0].Desc != "小红书示例笔记一" || items[0].MediaType != "video" {
		t.Fatalf("item[0] wrong: %+v", items[0])
	}
	if items[1].MediaType != "image" {
		t.Fatalf("item[1] media type = %q, want image (normal)", items[1].MediaType)
	}
	if items[0].Author.UID != "xhs-user-0001" || items[0].Author.Nickname != "小红书示例作者一" {
		t.Fatalf("author wrong: %+v", items[0].Author)
	}
	if sawKeyword != "露营" {
		t.Fatalf("keyword = %q", sawKeyword)
	}
}

// TestXhsDefaultsSmoke: only search/comments are declared.
func TestXhsDefaultsSmoke(t *testing.T) {
	d, reg, err := Defaults(contractsDir(t))
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	if d.Platform != "xhs" || d.UA == "" {
		t.Fatalf("platform/UA wrong: %+v", d)
	}
	if len(d.CookieNames) != 1 || d.CookieNames[0] != "web_session" {
		t.Fatalf("CookieNames = %v", d.CookieNames)
	}
	if d.Names["search"] != "xhs-search" || d.Names["comments"] != "xhs-comments" {
		t.Fatalf("Names = %v", d.Names)
	}
	for _, cat := range []string{"replies", "user", "group"} {
		if d.Names[cat] != "" {
			t.Fatalf("category %s should not be declared, got %q", cat, d.Names[cat])
		}
	}
	for cat, name := range d.Names {
		if name == "" {
			continue
		}
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("category %s maps to %q, not registered", cat, name)
		}
	}
	if d.SignerAs() != nil {
		t.Fatal("default Signer should be nil")
	}
}
