package xhs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

func mockEngine(t *testing.T, reg *contracts.Registry, names map[string]map[string]string) *collect.Engine {
	t.Helper()
	return collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Obs:      obs.NewCounterMap(),
		Cookies:  map[string]string{Platform: "web_session=test-session"},
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

	dir := testkit.ContractsDir(t, 3)
	d, _, _ := Defaults(dir)
	reg := testkit.RemapContracts(t, dir, srv, "xhs-comments")
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

	dir := testkit.ContractsDir(t, 3)
	d, _, _ := Defaults(dir)
	reg := testkit.RemapContracts(t, dir, srv, "xhs-search")
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
	d, reg, err := Defaults(testkit.ContractsDir(t, 3))
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	if d.Platform != "xhs" || d.UA == "" {
		t.Fatalf("platform/UA wrong: %+v", d)
	}
	if len(d.CookieNames) != 1 || d.CookieNames[0] != "web_session" {
		t.Fatalf("CookieNames = %v", d.CookieNames)
	}
	if d.Names["search"] != "xhs-search" || d.Names["comments"] != "xhs-comments" ||
		d.Names["user"] != "xhs-user" || d.Names["group"] != "xhs-group-members" {
		t.Fatalf("Names = %v", d.Names)
	}
	if d.Names["replies"] != "xhs-comments-replies" {
		t.Fatalf("replies should map to xhs-comments-replies, got %q", d.Names["replies"])
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

// TestXhsRepliesRealContract: the real xhs-comments-replies contract drives
// a mocked sub-comment page; comment_id travels as the placeholder and the
// data.comments records bind id/content/like_count + user_info author.
func TestXhsRepliesRealContract(t *testing.T) {
	var sawCommentID, sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawCommentID = r.URL.Query().Get("comment_id")
		_, _ = w.Write([]byte(`{
			"data": {
				"comments": [
					{"id": "xhs-subcomment-0001", "content": "小红书示例子评论一", "create_time": 1780006001, "like_count": "12",
					 "user_info": {"user_id": "xhs-user-0005", "nickname": "小红书回复者一", "avatar": "https://example.invalid/xhsr-1.jpg", "ip_location": "上海"}},
					{"id": "xhs-subcomment-0002", "content": "小红书示例子评论二", "create_time": 1780006002, "like_count": "3",
					 "user_info": {"user_id": "xhs-user-0006", "nickname": "小红书回复者二", "avatar": "https://example.invalid/xhsr-2.jpg", "ip_location": "浙江"}}
				],
				"has_more": false,
				"cursor": ""
			}
		}`))
	}))
	defer srv.Close()

	dir := testkit.ContractsDir(t, 3)
	d, _, _ := Defaults(dir)
	reg := testkit.RemapContracts(t, dir, srv, "xhs-comments-replies")
	eng := mockEngine(t, reg, map[string]map[string]string{Platform: d.Names})

	cmts, cur, err := eng.CommentReplies(context.Background(), Platform, "example-note-0001", "xhs-comment-0001", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("CommentReplies: %v", err)
	}
	if len(cmts) != 2 {
		t.Fatalf("replies = %d, want 2", len(cmts))
	}
	c0 := cmts[0]
	if c0.CID != "xhs-subcomment-0001" || c0.Text != "小红书示例子评论一" || c0.CreateTime != 1780006001 || c0.DiggCount != 12 {
		t.Fatalf("reply fields wrong: %+v", c0)
	}
	if c0.User.UID != "xhs-user-0005" || c0.User.Nickname != "小红书回复者一" || c0.User.IPLabel != "上海" ||
		c0.User.AvatarURL != "https://example.invalid/xhsr-1.jpg" {
		t.Fatalf("reply author wrong: %+v", c0.User)
	}
	if cur.HasMore {
		t.Fatalf("cursor = %+v, want single page", cur)
	}
	if sawPath != "/api/sns/web/v2/comment/sub/page" {
		t.Fatalf("path = %q, want the contract sub-comment endpoint", sawPath)
	}
	if sawCommentID != "xhs-comment-0001" {
		t.Fatalf("comment_id = %q", sawCommentID)
	}
}

// TestXhsGroupMembersRealContract: the xhs-group-members contract drives a
// mocked group-member enumeration; group_id travels as a query placeholder.
func TestXhsGroupMembersRealContract(t *testing.T) {
	var sawGroupID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawGroupID = r.URL.Query().Get("group_id")
		_, _ = w.Write([]byte(`{"members":[
			{"uid":"6x333333001","nickname":"小红书群成员一","joined_at":1780004001,"ip_label":"上海","gender":2}
		],"cursor":"","has_more":false}`))
	}))
	defer srv.Close()

	dir := testkit.ContractsDir(t, 3)
	d, _, _ := Defaults(dir)
	reg := testkit.RemapContracts(t, dir, srv, "xhs-group-members")
	eng := mockEngine(t, reg, map[string]map[string]string{Platform: d.Names})
	members, _, err := eng.GroupMembers(context.Background(), Platform, "g-x1", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	if members[0].UID != "6x333333001" || members[0].Nickname != "小红书群成员一" || members[0].JoinedAt != 1780004001 {
		t.Fatalf("member fields wrong: %+v", members[0])
	}
	if sawGroupID != "g-x1" {
		t.Fatalf("group_id param = %q", sawGroupID)
	}
}

// TestXhsUserProfileRealContract: the xhs-user contract drives a mocked
// profile lookup; user_id travels as a query placeholder.
func TestXhsUserProfileRealContract(t *testing.T) {
	var sawUID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUID = r.URL.Query().Get("sec_uid")
		_, _ = w.Write([]byte(`{"user_list":[{"uid":"6x111111001","nickname":"小红书用户一","follower_count":800}]}`))
	}))
	defer srv.Close()

	dir := testkit.ContractsDir(t, 3)
	d, _, _ := Defaults(dir)
	reg := testkit.RemapContracts(t, dir, srv, "xhs-user")
	eng := mockEngine(t, reg, map[string]map[string]string{Platform: d.Names})
	u, err := eng.UserProfile(context.Background(), Platform, "sec-x1")
	if err != nil {
		t.Fatalf("UserProfile: %v", err)
	}
	if u.UID != "6x111111001" || u.Nickname != "小红书用户一" || u.FollowerCount != 800 {
		t.Fatalf("profile = %+v", u)
	}
	if sawUID != "sec-x1" {
		t.Fatalf("sec_uid param = %q", sawUID)
	}
}
