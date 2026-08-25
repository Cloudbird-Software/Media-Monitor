package collect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

func addContracts(t *testing.T, cs ...*contracts.Contract) *contracts.Registry {
	t.Helper()
	reg := contracts.NewRegistry()
	for _, c := range cs {
		if err := reg.Add(c); err != nil {
			t.Fatalf("add contract %s: %v", c.Name, err)
		}
	}
	return reg
}

// mockEngine wires a registry + httptest-backed HTTP client into an engine.
func mockEngine(t *testing.T, reg *contracts.Registry, mutate func(*Context)) *Engine {
	t.Helper()
	ctx := Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Obs:      obs.NewCounterMap(),
	}
	if mutate != nil {
		mutate(&ctx)
	}
	return New(ctx)
}

func searchContract(srv *httptest.Server) *contracts.Contract {
	return &contracts.Contract{
		Name:     "mock-search",
		Platform: "mock",
		Category: "search",
		Version:  "1",
		Transport: contracts.Transport{
			BaseURL:      srv.URL,
			Path:         "/search",
			Method:       "GET",
			Placeholders: []string{"keyword"},
		},
		Binding: contracts.Binding{
			Items: "$.data",
			Fields: map[string]string{
				"id":              "$.data[].id",
				"desc":            "$.data[].desc",
				"create_time":     "$.data[].ts",
				"stats.digg":      "$.data[].digg",
				"author.nickname": "$.data[].author.nickname",
				"author.uid":      "$.data[].author.uid",
			},
		},
		Paging: contracts.Paging{
			CursorParam:    "cursor",
			CountParam:     "count",
			CountDefault:   20,
			HasMorePath:    "$.has_more",
			NextCursorPath: "$.cursor",
		},
	}
}

const (
	pageBody1 = `{"data":[
		{"id":"i1","desc":"d1","ts":11,"digg":111,"author":{"uid":"u1","nickname":"n1"}},
		{"id":"i2","desc":"d2","ts":22,"digg":222,"author":{"uid":"u2","nickname":"n2"}}
	],"cursor":"20","has_more":true}`
	pageBody2 = `{"data":[
		{"id":"i3","desc":"d3","ts":33,"digg":333,"author":{"uid":"u3","nickname":"n3"}},
		{"id":"i4","desc":"d4","ts":44,"digg":444,"author":{"uid":"u4","nickname":"n4"}}
	],"cursor":"","has_more":false}`
)

// TestSearchPaginationMergesPages: two has_more pages merge into one result
// list; the cursor travels back to the server on the second request.
func TestSearchPaginationMergesPages(t *testing.T) {
	var calls atomic.Int64
	var mu sync.Mutex
	var seenCursor []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("keyword") != "kw" {
			t.Errorf("keyword = %q", q.Get("keyword"))
		}
		mu.Lock()
		seenCursor = append(seenCursor, q.Get("cursor"))
		mu.Unlock()
		switch calls.Add(1) {
		case 1:
			_, _ = w.Write([]byte(pageBody1))
		default:
			_, _ = w.Write([]byte(pageBody2))
		}
	}))
	defer srv.Close()

	eng := mockEngine(t, addContracts(t, searchContract(srv)), func(c *Context) {
		c.Names = map[string]map[string]string{"mock": {"search": "mock-search", "replies": ""}}
	})
	items, cur, err := eng.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 0)
	if err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4 (merged)", len(items))
	}
	ids := []string{items[0].ID, items[1].ID, items[2].ID, items[3].ID}
	if strings.Join(ids, ",") != "i1,i2,i3,i4" {
		t.Fatalf("ids = %v", ids)
	}
	if items[1].Author.Nickname != "n2" || items[1].Author.UID != "u2" {
		t.Fatalf("author binding wrong: %+v", items[1].Author)
	}
	if items[0].CreateTime != 11 || items[1].Stats.Digg != 222 {
		t.Fatalf("field binding wrong: %+v", items[0])
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2", calls.Load())
	}
	if cur.Page != 2 || cur.HasMore {
		t.Fatalf("cursor = %+v, want Page 2 and no more pages", cur)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenCursor) != 2 || seenCursor[0] != "" || seenCursor[1] != "20" {
		t.Fatalf("cursor params seen = %v, want [\"\" \"20\"]", seenCursor)
	}
	if eng.obs.Get("collect.fetch") != 2 {
		t.Fatalf("collect.fetch counter = %d, want 2", eng.obs.Get("collect.fetch"))
	}
}

// TestSearchLimitTruncates: the limit is sent as the count param and the
// merged result is truncated to the limit across pages.
func TestSearchLimitTruncates(t *testing.T) {
	var calls atomic.Int64
	var counts []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		counts = append(counts, q.Get("count"))
		mu.Unlock()
		switch calls.Add(1) {
		case 1:
			_, _ = w.Write([]byte(pageBody1))
		default:
			_, _ = w.Write([]byte(pageBody2))
		}
	}))
	defer srv.Close()

	eng := mockEngine(t, addContracts(t, searchContract(srv)), func(c *Context) {
		c.Names = map[string]map[string]string{"mock": {"search": "mock-search"}}
	})
	items, _, err := eng.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 3)
	if err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (truncated across 2 pages)", len(items))
	}
	if items[0].ID != "i1" || items[2].ID != "i3" {
		t.Fatalf("truncation order wrong: %s %s %s", items[0].ID, items[1].ID, items[2].ID)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, c := range counts {
		if c != "3" {
			t.Fatalf("count param on request %d = %q, want 3", i+1, c)
		}
	}
}

// TestCommentsBindAuthor: comment records bind cid/text/counts and the
// author sub-object into model.Comment.User; the item id travels as a query
// param (placeholder with no path token → query router).
func TestCommentsBindAuthor(t *testing.T) {
	var qParam string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qParam = r.URL.Query().Get("aweme_id")
		_, _ = w.Write([]byte(`{"comments":[
			{"cid":"c1","aweme_id":"aw1","text":"t1","ct":1700000001,"digg":5,
			 "user":{"uid":"u-1","nickname":"nick-1","ip_label":"广东","gender":2}}
		]}`))
	}))
	defer srv.Close()

	c := &contracts.Contract{
		Name:     "mock-comments",
		Platform: "mock",
		Category: "comments",
		Version:  "1",
		Transport: contracts.Transport{
			BaseURL:      srv.URL,
			Path:         "/comments",
			Method:       "GET",
			Placeholders: []string{"aweme_id"},
		},
		Binding: contracts.Binding{
			Comments: "$.comments",
			Fields: map[string]string{
				"cid":           "$.comments[].cid",
				"text":          "$.comments[].text",
				"create_time":   "$.comments[].ct",
				"digg_count":    "$.comments[].digg",
				"user.uid":      "$.comments[].user.uid",
				"user.nickname": "$.comments[].user.nickname",
				"user.ip_label": "$.comments[].user.ip_label",
				"user.gender":   "$.comments[].user.gender",
			},
		},
		Paging: contracts.Paging{CursorParam: "cursor", CountParam: "count", CountDefault: 20},
	}
	eng := mockEngine(t, addContracts(t, c), func(cc *Context) {
		cc.Names = map[string]map[string]string{"mock": {"comments": "mock-comments"}}
	})
	cmts, _, err := eng.ItemComments(context.Background(), "mock", "aw-1", model.Cursor{}, 10)
	if err != nil {
		t.Fatalf("ItemComments: %v", err)
	}
	if len(cmts) != 1 {
		t.Fatalf("comments = %d, want 1", len(cmts))
	}
	cm := cmts[0]
	if cm.CID != "c1" || cm.Text != "t1" || cm.AwemeID != "aw1" || cm.CreateTime != 1700000001 || cm.DiggCount != 5 {
		t.Fatalf("comment fields wrong: %+v", cm)
	}
	if cm.User.UID != "u-1" || cm.User.Nickname != "nick-1" || cm.User.IPLabel != "广东" || cm.User.Gender != 2 {
		t.Fatalf("author binding wrong: %+v", cm.User)
	}
	if qParam != "aw-1" {
		t.Fatalf("aweme_id query param = %q, want aw-1", qParam)
	}
}

// TestBindFailClosed: a missing binding path errors; an empty list is a
// valid zero-record page (paging metadata continues/exhausts normally).
func TestBindFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"other": 1}`))
	}))
	eng := mockEngine(t, addContracts(t, searchContract(srv)), func(c *Context) {
		c.Names = map[string]map[string]string{"mock": {"search": "mock-search"}}
	})
	if _, _, err := eng.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 10); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing binding: err = %v, want missing-path error", err)
	}
	srv.Close()

	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": []}`))
	}))
	defer emptySrv.Close()
	eng2 := mockEngine(t, addContracts(t, searchContract(emptySrv)), func(c *Context) {
		c.Names = map[string]map[string]string{"mock": {"search": "mock-search"}}
	})
	items, cur, err := eng2.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 10)
	if err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if len(items) != 0 || cur.HasMore {
		t.Fatalf("empty page: items=%d has_more=%v", len(items), cur.HasMore)
	}
}

// TestRepliesContractNotDeclared: the explicit error contract for the
// replies surface (landing in a later PR).
func TestRepliesContractNotDeclared(t *testing.T) {
	eng := mockEngine(t, addContracts(t), nil)
	_, _, err := eng.CommentReplies(context.Background(), "mock", "item-1", "cid-1", model.Cursor{}, 20)
	if err == nil || err.Error() != "replies contract not declared" {
		t.Fatalf("err = %v, want %q", err, "replies contract not declared")
	}
}

// TestSignerMergedIntoQuery: the per-platform signer sees the pre-signature
// URL and params, and its output lands in the final query.
func TestSignerMergedIntoQuery(t *testing.T) {
	var sawSig, sawContract string
	sawParams := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSig = r.URL.Query().Get("x_sign")
		_, _ = w.Write([]byte(`{"data":[{"id":"s1","desc":"sd","media_type":"video"}]}`))
	}))
	defer srv.Close()
	c := searchContract(srv)
	c.Signature = contracts.Signature{Params: []string{"x_sign", "msToken"}, Required: []string{"x_sign"}}

	eng := mockEngine(t, addContracts(t, c), func(cc *Context) {
		cc.Names = map[string]map[string]string{"mock": {"search": "mock-search"}}
		cc.Signers = map[string]httpclient.Signer{"mock": httpclient.StaticSigner{
			Fn: func(ctx context.Context, contractName, rawURL string, params map[string]string) (map[string]string, error) {
				sawContract = contractName
				for k, v := range params {
					sawParams[k] = v
				}
				return map[string]string{"x_sign": "sig-1", "msToken": "tok-1"}, nil
			},
		}}
	})
	items, _, err := eng.SearchItems(context.Background(), "mock", "kw", "video", model.Cursor{}, 10)
	if err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	if sawSig != "sig-1" {
		t.Fatalf("server saw x_sign = %q", sawSig)
	}
	if sawContract != "mock-search" {
		t.Fatalf("signer contract = %q", sawContract)
	}
	if sawParams["keyword"] != "kw" || sawParams["type"] != "video" {
		t.Fatalf("signer params = %v", sawParams)
	}
}

// TestSignatureRequiredFailClosed: a signer that omits a required signature
// parameter blocks the fetch.
func TestSignatureRequiredFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"s1"}]}`))
	}))
	defer srv.Close()
	c := searchContract(srv)
	c.Signature = contracts.Signature{Required: []string{"a_bogus"}}

	eng := mockEngine(t, addContracts(t, c), func(cc *Context) {
		cc.Names = map[string]map[string]string{"mock": {"search": "mock-search"}}
		cc.Signers = map[string]httpclient.Signer{"mock": httpclient.StaticSigner{
			Fn: func(ctx context.Context, contractName, rawURL string, params map[string]string) (map[string]string, error) {
				return map[string]string{"other": "x"}, nil
			},
		}}
	})
	_, _, err := eng.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 10)
	if err == nil || !strings.Contains(err.Error(), "a_bogus") {
		t.Fatalf("err = %v, want missing a_bogus error", err)
	}
}

// TestCookieHeaderMerged: the platform cookie fragment travels as the
// Cookie request header.
func TestCookieHeaderMerged(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		_, _ = w.Write([]byte(`{"data":[{"id":"c1"}]}`))
	}))
	defer srv.Close()

	eng := mockEngine(t, addContracts(t, searchContract(srv)), func(c *Context) {
		c.Names = map[string]map[string]string{"mock": {"search": "mock-search"}}
		c.Cookies = map[string]string{"mock": "ttwid=aaa; sessionid=bbb"}
	})
	if _, _, err := eng.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 10); err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if gotCookie != "ttwid=aaa; sessionid=bbb" {
		t.Fatalf("Cookie header = %q", gotCookie)
	}
}

// TestUserProfileFlow: the sec_uid reaches the server as a query param and
// the first users-binding record becomes the profile.
func TestUserProfileFlow(t *testing.T) {
	var sawUID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUID = r.URL.Query().Get("sec_uid")
		_, _ = w.Write([]byte(`{"user_list":[{"uid":"1001","sec_uid":"sec-1","nickname":"作者一","follower_count":1200}]}`))
	}))
	defer srv.Close()

	c := &contracts.Contract{
		Name:      "mock-user",
		Platform:  "mock",
		Category:  "user",
		Version:   "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/user", Method: "GET"},
		Binding: contracts.Binding{
			Users: "$.user_list",
			Fields: map[string]string{
				"uid":            "$.user_list[].uid",
				"sec_uid":        "$.user_list[].sec_uid",
				"nickname":       "$.user_list[].nickname",
				"follower_count": "$.user_list[].follower_count",
			},
		},
	}
	eng := mockEngine(t, addContracts(t, c), func(cc *Context) {
		cc.Names = map[string]map[string]string{"mock": {"user": "mock-user"}}
	})
	u, err := eng.UserProfile(context.Background(), "mock", "sec-1")
	if err != nil {
		t.Fatalf("UserProfile: %v", err)
	}
	if u.UID != "1001" || u.SecUID != "sec-1" || u.Nickname != "作者一" || u.FollowerCount != 1200 {
		t.Fatalf("profile = %+v", u)
	}
	if sawUID != "sec-1" {
		t.Fatalf("sec_uid param = %q", sawUID)
	}
}

// TestGroupMembersPagination: members flow with per-page cursor and the
// group id forwarded through the placeholder (query routing).
func TestGroupMembersPagination(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("group_id") != "g-1" {
			t.Errorf("group_id param = %q", r.URL.Query().Get("group_id"))
		}
		switch calls.Add(1) {
		case 1:
			_, _ = w.Write([]byte(`{"members":[
				{"uid":"m1","nickname":"成员一","joined_at":1780000001},
				{"uid":"m2","nickname":"成员二","joined_at":1780000002}
			],"cursor":"c2","has_more":true}`))
		default:
			_, _ = w.Write([]byte(`{"members":[
				{"uid":"m3","nickname":"成员三","joined_at":1780000003}
			],"cursor":"","has_more":false}`))
		}
	}))
	defer srv.Close()

	c := &contracts.Contract{
		Name:     "mock-group",
		Platform: "mock",
		Category: "group_members",
		Version:  "1",
		Transport: contracts.Transport{
			BaseURL:      srv.URL,
			Path:         "/group",
			Method:       "GET",
			Placeholders: []string{"group_id"},
		},
		Binding: contracts.Binding{
			Members: "$.members",
			Fields: map[string]string{
				"uid":       "$.members[].uid",
				"nickname":  "$.members[].nickname",
				"joined_at": "$.members[].joined_at",
			},
		},
		Paging: contracts.Paging{
			CursorParam: "cursor", CountParam: "count", CountDefault: 20,
			HasMorePath: "$.has_more", NextCursorPath: "$.cursor",
		},
	}
	eng := mockEngine(t, addContracts(t, c), func(cc *Context) {
		cc.Names = map[string]map[string]string{"mock": {"group": "mock-group"}}
	})
	members, cur, err := eng.GroupMembers(context.Background(), "mock", "g-1", model.Cursor{}, 0)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("members = %d, want 3", len(members))
	}
	for i, m := range members {
		if m.GroupID != "g-1" {
			t.Fatalf("member %d group id = %q", i, m.GroupID)
		}
		if m.UID != "m3" && m.UID == "" {
			t.Fatalf("member %d uid empty", i)
		}
	}
	if members[0].Nickname != "成员一" || members[0].JoinedAt != 1780000001 {
		t.Fatalf("member fields wrong: %+v", members[0])
	}
	if cur.Page != 2 || cur.HasMore {
		t.Fatalf("cursor = %+v", cur)
	}
}

// TestPostBodyContract: POST contracts send transport body + caller query
// merged as JSON and the Content-Type header.
func TestPostBodyContract(t *testing.T) {
	var bodyMap map[string]any
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&bodyMap)
		_, _ = w.Write([]byte(`{"data":[{"id":"p1","desc":"post-item"}]}`))
	}))
	defer srv.Close()

	c := &contracts.Contract{
		Name:     "mock-post",
		Platform: "mock",
		Category: "search",
		Version:  "1",
		Transport: contracts.Transport{
			BaseURL: srv.URL, Path: "/graphql", Method: "POST",
			Body: map[string]any{"operationName": "mockOp"},
		},
		Binding: contracts.Binding{Items: "$.data"},
	}
	eng := mockEngine(t, addContracts(t, c), func(cc *Context) {
		cc.Names = map[string]map[string]string{"mock": {"search": "mock-post"}}
	})
	items, _, err := eng.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 10)
	if err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "p1" || items[0].Desc != "post-item" {
		t.Fatalf("items = %+v", items)
	}
	if bodyMap["operationName"] != "mockOp" || bodyMap["keyword"] != "kw" {
		t.Fatalf("body = %v", bodyMap)
	}
	if !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("Content-Type = %q", contentType)
	}
}

// TestPaginationLoopGuard: a cursor that never advances aborts after the
// page cap instead of looping forever.
func TestPaginationLoopGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"x"}],"cursor":"20","has_more":true}`))
	}))
	defer srv.Close()

	eng := mockEngine(t, addContracts(t, searchContract(srv)), func(c *Context) {
		c.Names = map[string]map[string]string{"mock": {"search": "mock-search"}}
	})
	_, _, err := eng.SearchItems(context.Background(), "mock", "kw", "", model.Cursor{}, 0)
	if err == nil || !strings.Contains(err.Error(), "pagination") {
		t.Fatalf("err = %v, want pagination cap error", err)
	}
}
