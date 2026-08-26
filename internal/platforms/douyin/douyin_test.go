package douyin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

// contractsDir resolves the repository adapt/contracts dir from this test
// package (three levels up: douyin → platforms → internal → root).
func contractsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..", "..", "adapt", "contracts")
}

// remapContracts loads the real contracts from dir and re-registers the
// listed ones with the httptest server as base URL (zero external network).
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

// TestDouyinCommentsRealContract: the actual douyin-comments contract
// drives a two-page mocked flow through the engine; default field paths bind
// the fixture-shaped records (cid/text/author profile).
func TestDouyinCommentsRealContract(t *testing.T) {
	var calls atomic.Int64
	var mu atomic.Value // string, cursor param seen on request 2
	mu.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("aweme_id") != "7660000000000000001" {
			t.Errorf("aweme_id = %q", q.Get("aweme_id"))
		}
		if q.Get("device_platform") != "webapp" || q.Get("aid") != "6383" {
			t.Errorf("static query lost: %v", r.URL.RawQuery)
		}
		if q.Get("count") != "20" {
			t.Errorf("count = %q", q.Get("count"))
		}
		cookie := r.Header.Get("Cookie")
		if !strings.Contains(cookie, "ttwid=test-ttwid") {
			t.Errorf("Cookie = %q, want ttwid present", cookie)
		}
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{
				"comments": [
					{"cid":"7660010000000000000001","aweme_id":"7660000000000000001","text":"示例评论一","create_time":1780001001,"digg_count":626,"reply_count":7,"sticky":false,
					 "user":{"uid":"1000000001","sec_uid":"MS4wLjABAAAA-c1","short_id":"100001","nickname":"示例用户一","avatar_url":"https://example.invalid/a1.jpg","signature":"个签一","ip_label":"陕西","gender":2,"follower_count":1200,"following_count":88,"aweme_count":45,"total_favorited":99000}},
					{"cid":"7660010000000000000002","aweme_id":"7660000000000000001","text":"示例评论二","create_time":1780001002,"digg_count":14,"reply_count":3,"sticky":false,
					 "user":{"uid":"1000000002","sec_uid":"MS4wLjABAAAA-c2","nickname":"示例用户二","avatar_url":"https://example.invalid/a2.jpg","ip_label":"浙江","gender":1}}
				],
				"cursor": 2, "has_more": true
			}`))
			return
		}
		mu.Store(q.Get("cursor"))
		_, _ = w.Write([]byte(`{
			"comments": [
				{"cid":"7660010000000000000003","aweme_id":"7660000000000000001","text":"示例评论三","create_time":1780001003,"digg_count":1,
				 "user":{"uid":"1000000003","nickname":"示例用户三","ip_label":"北京"}}
			],
			"cursor": 3, "has_more": false
		}`))
	}))
	defer srv.Close()

	dir := contractsDir(t)
	d, _, err := Defaults(dir)
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	// The real douyin-comments contract requires a_bogus in the final URL
	// (signature.required): inject the platform signer hook.
	d.Signer = func(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error) {
		return map[string]string{"a_bogus": "ab-" + contractName, "msToken": "tok-1"}, nil
	}
	reg := remapContracts(t, dir, srv, "douyin-comments")
	eng := collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Obs:      obs.NewCounterMap(),
		Signers:  map[string]httpclient.Signer{Platform: d.SignerAs()},
		Cookies:  map[string]string{Platform: "ttwid=test-ttwid; sessionid=s1"},
		Names:    map[string]map[string]string{Platform: d.Names},
	})
	cmts, cur, err := eng.ItemComments(context.Background(), Platform, "7660000000000000001", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("ItemComments: %v", err)
	}
	if len(cmts) != 3 {
		t.Fatalf("comments = %d, want 3 across two pages", len(cmts))
	}
	c0 := cmts[0]
	if c0.CID != "7660010000000000000001" || c0.Text != "示例评论一" || c0.DiggCount != 626 || c0.ReplyCount != 7 {
		t.Fatalf("comment fields wrong: %+v", c0)
	}
	// All 12 UserProfile fields (uid/sec_uid/short_id/nickname/avatar_url/
	// signature/ip_label/gender/follower_count/following_count/aweme_count/
	// total_favorited) must bind from the fixture-shaped author object.
	if c0.User.UID != "1000000001" || c0.User.SecUID != "MS4wLjABAAAA-c1" || c0.User.ShortID != "100001" ||
		c0.User.Nickname != "示例用户一" || c0.User.AvatarURL != "https://example.invalid/a1.jpg" ||
		c0.User.Signature != "个签一" || c0.User.IPLabel != "陕西" || c0.User.Gender != 2 ||
		c0.User.FollowerCount != 1200 || c0.User.FollowingCount != 88 ||
		c0.User.AwemeCount != 45 || c0.User.TotalFavorited != 99000 {
		t.Fatalf("author binding wrong: %+v", c0.User)
	}
	if cmts[1].User.Gender != 1 {
		t.Fatalf("second author gender = %d", cmts[1].User.Gender)
	}
	if cur.Page != 2 || cur.HasMore {
		t.Fatalf("cursor = %+v, want page 2, has_more false", cur)
	}
	if c := mu.Load().(string); c != "2" {
		t.Fatalf("page-2 cursor param = %q, want 2 (next_cursor_path)", c)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2", calls.Load())
	}
}

// TestDouyinSignatureRequiredFailClosed: without an injected signer the
// douyin contract's signature.Required (a_bogus) blocks the fetch.
func TestDouyinSignatureRequiredFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"comments":[{"cid":"c1"}]}`))
	}))
	defer srv.Close()

	dir := contractsDir(t)
	d, _, _ := Defaults(dir)
	reg := remapContracts(t, dir, srv, "douyin-comments")
	eng := collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Names:    map[string]map[string]string{Platform: d.Names},
	})
	_, _, err := eng.ItemComments(context.Background(), Platform, "7660000000000000001", model.Cursor{}, 20)
	if err == nil || !strings.Contains(err.Error(), "a_bogus") {
		t.Fatalf("err = %v, want missing a_bogus error", err)
	}
}

// TestDouyinCookieRequiredFailClosed: when the contract declares a required
// cookie (ttwid) and the request carries none, the engine fails closed with a
// missing-cookie error — no network call is made.
func TestDouyinCookieRequiredFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be called when required cookie is missing")
	}))
	defer srv.Close()

	dir := contractsDir(t)
	d, _, _ := Defaults(dir)
	d.Signer = func(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error) {
		return map[string]string{"a_bogus": "ab-" + contractName, "msToken": "tok-1"}, nil
	}
	reg := remapContracts(t, dir, srv, "douyin-comments")
	eng := collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Signers:  map[string]httpclient.Signer{Platform: d.SignerAs()},
		// No Cookies entry for Platform: the required ttwid is absent.
		Names: map[string]map[string]string{Platform: d.Names},
	})
	_, _, err := eng.ItemComments(context.Background(), Platform, "7660000000000000001", model.Cursor{}, 20)
	if err == nil || !strings.Contains(err.Error(), "ttwid") || !strings.Contains(err.Error(), "cookie") {
		t.Fatalf("err = %v, want missing-cookie (ttwid) error", err)
	}
}

// TestDouyinSignerHook: the injected Signer hook's output is merged into the
// outgoing query and satisfies the contract requirement.
func TestDouyinSignerHook(t *testing.T) {
	var sawSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSig = r.URL.Query().Get("a_bogus")
		_, _ = w.Write([]byte(`{"comments":[{"cid":"c1","text":"t"}]}`))
	}))
	defer srv.Close()

	dir := contractsDir(t)
	d, _, _ := Defaults(dir)
	d.Signer = func(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error) {
		return map[string]string{"a_bogus": "sig-from-hook", "msToken": "tok-1"}, nil
	}
	if d.SignerAs() == nil {
		t.Fatal("SignerAs() = nil for populated hook")
	}
	reg := remapContracts(t, dir, srv, "douyin-comments")
	eng := collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Signers:  map[string]httpclient.Signer{Platform: d.SignerAs()},
		Cookies:  map[string]string{Platform: "ttwid=test-ttwid"},
		Names:    map[string]map[string]string{Platform: d.Names},
	})
	if _, _, err := eng.ItemComments(context.Background(), Platform, "7660000000000000001", model.Cursor{}, 20); err != nil {
		t.Fatalf("ItemComments: %v", err)
	}
	if sawSig != "sig-from-hook" {
		t.Fatalf("a_bogus = %q", sawSig)
	}
}

// TestDouyinDefaultsSmoke: default assembly resolves against the real
// contract registry.
func TestDouyinDefaultsSmoke(t *testing.T) {
	d, reg, err := Defaults(contractsDir(t))
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	if d.Platform != "douyin" || d.UA == "" {
		t.Fatalf("platform/UA wrong: %+v", d)
	}
	if len(d.CookieNames) == 0 || d.CookieNames[0] != "ttwid" {
		t.Fatalf("CookieNames = %v", d.CookieNames)
	}
	for cat, name := range d.Names {
		if name == "" {
			continue
		}
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("category %s maps to %q, not registered", cat, name)
		}
	}
	if d.Names["search"] != "douyin-search" || d.Names["comments"] != "douyin-comments" ||
		d.Names["user"] != "douyin-user" || d.Names["group"] != "douyin-group-members" {
		t.Fatalf("Names = %v", d.Names)
	}
	if d.Names["replies"] != "douyin-comments-replies" {
		t.Fatalf("replies should map to douyin-comments-replies, got %q", d.Names["replies"])
	}
	if d.Signer != nil || d.SignerAs() != nil {
		t.Fatal("default Signer should be nil")
	}
}

// TestDouyinRepliesRealContract: the actual douyin-comments-replies contract
// drives a single-page mocked flow through the engine; comment_id travels as
// the placeholder and reply records bind with author profile + reply_to_cid.
func TestDouyinRepliesRealContract(t *testing.T) {
	var sawCommentID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCommentID = r.URL.Query().Get("comment_id")
		if r.URL.Query().Get("device_platform") != "webapp" || r.URL.Query().Get("aid") != "6383" {
			t.Errorf("static query lost: %v", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{
			"comments": [
				{"cid":"7660020000000000000001","aweme_id":"7660000000000000001","text":"示例回复一","create_time":1780002001,"digg_count":31,"reply_count":0,"sticky":false,"reply_to_cid":"7660010000000000000001",
				 "user":{"uid":"1000000011","sec_uid":"MS4wLjABAAAA-r1","short_id":"100011","nickname":"回复用户一","avatar_url":"https://example.invalid/ar1.jpg","signature":"个签","ip_label":"广东","gender":1,"follower_count":300,"following_count":40,"aweme_count":10,"total_favorited":5000}},
				{"cid":"7660020000000000000002","aweme_id":"7660000000000000001","text":"示例回复二","create_time":1780002002,"digg_count":5,"reply_count":0,"sticky":false,"reply_to_cid":"7660010000000000000001",
				 "user":{"uid":"1000000012","sec_uid":"MS4wLjABAAAA-r2","nickname":"回复用户二","ip_label":"江苏","gender":2}}
			],
			"cursor": 2, "has_more": false
		}`))
	}))
	defer srv.Close()

	dir := contractsDir(t)
	d, _, err := Defaults(dir)
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	d.Signer = func(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error) {
		return map[string]string{"a_bogus": "ab-" + contractName, "msToken": "tok-1"}, nil
	}
	reg := remapContracts(t, dir, srv, "douyin-comments-replies")
	eng := collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Obs:      obs.NewCounterMap(),
		Signers:  map[string]httpclient.Signer{Platform: d.SignerAs()},
		Cookies:  map[string]string{Platform: "ttwid=test-ttwid; sessionid=s1"},
		Names:    map[string]map[string]string{Platform: d.Names},
	})
	cmts, _, err := eng.CommentReplies(context.Background(), Platform, "7660000000000000001", "7660010000000000000001", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("CommentReplies: %v", err)
	}
	if len(cmts) != 2 {
		t.Fatalf("replies = %d, want 2", len(cmts))
	}
	c0 := cmts[0]
	if c0.CID != "7660020000000000000001" || c0.Text != "示例回复一" || c0.DiggCount != 31 || c0.ReplyToCID != "7660010000000000000001" {
		t.Fatalf("reply fields wrong: %+v", c0)
	}
	if c0.User.UID != "1000000011" || c0.User.Nickname != "回复用户一" || c0.User.IPLabel != "广东" ||
		c0.User.Gender != 1 || c0.User.FollowerCount != 300 || c0.User.AwemeCount != 10 || c0.User.TotalFavorited != 5000 {
		t.Fatalf("reply author binding wrong: %+v", c0.User)
	}
	if cmts[1].User.Gender != 2 {
		t.Fatalf("second reply author gender = %d", cmts[1].User.Gender)
	}
	if sawCommentID != "7660010000000000000001" {
		t.Fatalf("comment_id param = %q, want the top-level cid", sawCommentID)
	}
}

// TestDouyinRepliesContractNotDeclared: when a platform declares no replies
// contract, the engine surfaces the explicit contract error (no network call).
func TestDouyinRepliesContractNotDeclared(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be called for undeclared replies")
	}))
	defer srv.Close()
	dir := contractsDir(t)
	d, _, _ := Defaults(dir)
	// Force replies undeclared to exercise the resolver error path.
	d.Names["replies"] = ""
	reg := remapContracts(t, dir, srv, "douyin-comments")
	eng := collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"test-ua"}}),
		Names:    map[string]map[string]string{Platform: d.Names},
	})
	_, _, err := eng.CommentReplies(context.Background(), Platform, "item-1", "cid-1", model.Cursor{}, 20)
	if err == nil || err.Error() != "replies contract not declared" {
		t.Fatalf("err = %v, want %q", err, "replies contract not declared")
	}
}
