package kuaishou

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		Cookies:  map[string]string{Platform: "did=test-did"},
		Names:    names,
	})
}

// TestKuaishouSearchPostFlow: the real kuaishou-search contract is a POST;
// the keyword and static body travel as JSON, and the feed shape binds into
// model.Item via the default field paths (photo.id / photo.caption / ...).
func TestKuaishouSearchPostFlow(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{
			"data": {"visionSearchPhotoResult": {"feeds": [
				{"photo": {"id": "3x8-example-photo-0001", "caption": "快手示例视频一", "timestamp": 1780003001, "view_count": 50000, "like_count": 4000},
				 "author": {"id": "author-0001", "name": "快手示例作者一"}},
				{"photo": {"id": "3x8-example-photo-0002", "caption": "快手示例视频二", "timestamp": 1780003002, "view_count": 900, "like_count": 120},
				 "author": {"id": "author-0002", "name": "快手示例作者二"}}
			]}}
		}`))
	}))
	defer srv.Close()

	dir := testkit.ContractsDir(t, 3)
	d, _, _ := Defaults(dir)
	reg := testkit.RemapContracts(t, dir, srv, "kuaishou-search")
	eng := mockEngine(t, reg, map[string]map[string]string{Platform: d.Names})

	items, _, err := eng.SearchItems(context.Background(), Platform, "智能家居", "", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].ID != "3x8-example-photo-0001" || items[0].Desc != "快手示例视频一" || items[0].CreateTime != 1780003001 {
		t.Fatalf("item fields wrong: %+v", items[0])
	}
	if items[0].Stats.Digg != 4000 {
		t.Fatalf("digg = %d, want 4000", items[0].Stats.Digg)
	}
	if items[0].Author.UID != "author-0001" || items[0].Author.Nickname != "快手示例作者一" {
		t.Fatalf("author wrong: %+v", items[0].Author)
	}
	if gotBody["operationName"] != "visionSearchPhoto" {
		t.Fatalf("operationName = %v", gotBody["operationName"])
	}
	if gotBody["keyword"] != "智能家居" {
		t.Fatalf("keyword = %v", gotBody["keyword"])
	}
}

// TestKuaishouCommentsFlow: the real kuaishou-comments contract binds
// comment_list records (comment_id/content/liked_count, author object).
func TestKuaishouCommentsFlow(t *testing.T) {
	var sawPhotoID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPhotoID = r.URL.Query().Get("photo_id")
		_, _ = w.Write([]byte(`{
			"comment_list": [
				{"comment_id": "ks-comment-0001", "content": "快手示例评论一", "create_time": 1780004001, "liked_count": 12,
				 "author": {"id": "ks-author-0001", "name": "快手评论者一", "head_url": "https://example.invalid/ks-1.jpg"}}
			],
			"cursor": "2"
		}`))
	}))
	defer srv.Close()

	dir := testkit.ContractsDir(t, 3)
	d, _, _ := Defaults(dir)
	reg := testkit.RemapContracts(t, dir, srv, "kuaishou-comments")
	eng := mockEngine(t, reg, map[string]map[string]string{Platform: d.Names})

	cmts, _, err := eng.ItemComments(context.Background(), Platform, "3x8-comment-item", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("ItemComments: %v", err)
	}
	if len(cmts) != 1 {
		t.Fatalf("comments = %d", len(cmts))
	}
	cm := cmts[0]
	if cm.CID != "ks-comment-0001" || cm.Text != "快手示例评论一" || cm.CreateTime != 1780004001 || cm.DiggCount != 12 {
		t.Fatalf("comment fields wrong: %+v", cm)
	}
	if cm.User.UID != "ks-author-0001" || cm.User.Nickname != "快手评论者一" || cm.User.AvatarURL != "https://example.invalid/ks-1.jpg" {
		t.Fatalf("author wrong: %+v", cm.User)
	}
	if sawPhotoID != "3x8-comment-item" {
		t.Fatalf("photo_id = %q", sawPhotoID)
	}
}

// TestKuaishouDefaultsSmoke: only search/comments are declared.
func TestKuaishouDefaultsSmoke(t *testing.T) {
	d, reg, err := Defaults(testkit.ContractsDir(t, 3))
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	if d.Platform != "kuaishou" || d.UA == "" {
		t.Fatalf("platform/UA wrong: %+v", d)
	}
	if len(d.CookieNames) != 1 || d.CookieNames[0] != "did" {
		t.Fatalf("CookieNames = %v", d.CookieNames)
	}
	if d.Names["search"] != "kuaishou-search" || d.Names["comments"] != "kuaishou-comments" ||
		d.Names["user"] != "kuaishou-user" || d.Names["group"] != "kuaishou-group-members" {
		t.Fatalf("Names = %v", d.Names)
	}
	if d.Names["replies"] != "" {
		t.Fatalf("replies should not be declared, got %q", d.Names["replies"])
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

// TestKuaishouGroupMembersRealContract: the kuaishou-group-members contract
// drives a mocked group-member enumeration; group_id travels as a query
// placeholder and member records bind with joined_at.
func TestKuaishouGroupMembersRealContract(t *testing.T) {
	var sawGroupID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawGroupID = r.URL.Query().Get("group_id")
		_, _ = w.Write([]byte(`{"members":[
			{"uid":"3x222222001","nickname":"快手群成员一","joined_at":1780003001,"ip_label":"四川","gender":1},
			{"uid":"3x222222002","nickname":"快手群成员二","joined_at":1780003002}
		],"cursor":"","has_more":false}`))
	}))
	defer srv.Close()

	dir := testkit.ContractsDir(t, 3)
	d, _, _ := Defaults(dir)
	reg := testkit.RemapContracts(t, dir, srv, "kuaishou-group-members")
	eng := mockEngine(t, reg, map[string]map[string]string{Platform: d.Names})
	members, _, err := eng.GroupMembers(context.Background(), Platform, "g-k1", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	if members[0].UID != "3x222222001" || members[0].Nickname != "快手群成员一" || members[0].JoinedAt != 1780003001 {
		t.Fatalf("member fields wrong: %+v", members[0])
	}
	if members[0].IPLabel != "四川" || members[0].Gender != 1 {
		t.Fatalf("member binding wrong: %+v", members[0])
	}
	if sawGroupID != "g-k1" {
		t.Fatalf("group_id param = %q", sawGroupID)
	}
}

// TestKuaishouUserProfileRealContract: the kuaishou-user contract drives a
// mocked profile lookup; sec_uid travels as a query placeholder.
func TestKuaishouUserProfileRealContract(t *testing.T) {
	var sawUID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUID = r.URL.Query().Get("sec_uid")
		_, _ = w.Write([]byte(`{"user_list":[{"uid":"3x111111001","short_id":"K100001","nickname":"快手用户一","follower_count":3200}]}`))
	}))
	defer srv.Close()

	dir := testkit.ContractsDir(t, 3)
	d, _, _ := Defaults(dir)
	reg := testkit.RemapContracts(t, dir, srv, "kuaishou-user")
	eng := mockEngine(t, reg, map[string]map[string]string{Platform: d.Names})
	u, err := eng.UserProfile(context.Background(), Platform, "sec-k1")
	if err != nil {
		t.Fatalf("UserProfile: %v", err)
	}
	if u.UID != "3x111111001" || u.ShortID != "K100001" || u.Nickname != "快手用户一" || u.FollowerCount != 3200 {
		t.Fatalf("profile = %+v", u)
	}
	if sawUID != "sec-k1" {
		t.Fatalf("sec_uid param = %q", sawUID)
	}
}
