package collect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
)

// enrichFixture serves one dy-shaped comments page (two comments, one shared
// author) plus the user-enrich face keyed by sec_uid.
type enrichFixture struct {
	commentReqs int64
	userReqs    int64
	many        bool         // serve 6 comments with 6 distinct authors
	userMode    atomic.Value // string: "ok" | "500"
}

func (f *enrichFixture) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/aweme/v1/web/comment/list/":
			atomic.AddInt64(&f.commentReqs, 1)
			if f.many {
				var rows string
				for i := 1; i <= 6; i++ {
					if i > 1 {
						rows += ","
					}
					rows += fmt.Sprintf(`{"cid":"c%d","text":"t","ip_label":"四川","user":{"uid":"10%02d","sec_uid":"MS4u%d","short_id":"1","nickname":"n%d","avatar_thumb":{"url_list":["https://a/%d.jpg"]}}}`, i, i, i, i, i)
				}
				fmt.Fprintf(w, `{"status_code":0,"has_more":0,"cursor":0,"comments":[%s]}`, rows)
				return
			}
			fmt.Fprint(w, `{"status_code":0,"has_more":0,"cursor":0,"comments":[`+
				`{"cid":"c1","text":"t1","ip_label":"四川","user":{"uid":"1001","sec_uid":"MS4u1","short_id":"11","nickname":"payload-nick","avatar_thumb":{"url_list":["https://a/1.jpg"]}}},`+
				`{"cid":"c2","text":"t2","ip_label":"四川","user":{"uid":"1001","sec_uid":"MS4u1","short_id":"11","nickname":"payload-nick","avatar_thumb":{"url_list":["https://a/1.jpg"]}}}]}`)
		case "/aweme/v1/web/user/profile/other/":
			atomic.AddInt64(&f.userReqs, 1)
			if f.userMode.Load() == "500" {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"status_code":8}`)
				return
			}
			fmt.Fprint(w, `{"status_code":0,"user":{"uid":"1001","sec_uid":"MS4u1"},"user_list":[{`+
				`"uid":"1001","sec_uid":"MS4u1","short_id":"999","nickname":"enrich-nick","avatar_url":"https://a/e.jpg",`+
				`"signature":"bio","ip_label":"","gender":0,`+
				`"follower_count":1200,"following_count":88,"aweme_count":45,"total_favorited":99000}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func enrichTestEngine(srvURL string, o *obs.CounterMap) *Engine {
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "douyin-comments", Platform: douyin.Platform, Category: "comments", Version: "1",
		Transport: contracts.Transport{BaseURL: srvURL, Path: "/aweme/v1/web/comment/list/", Method: http.MethodGet, Placeholders: []string{"aweme_id"}},
		Binding: contracts.Binding{
			Comments: "$.comments",
			Fields: map[string]string{
				"user.avatar_url": "$.user.avatar_thumb.url_list[0]",
				"user.ip_label":   "$.ip_label",
			},
		},
		Paging: contracts.Paging{HasMorePath: "$.has_more"},
	})
	reg.Add(&contracts.Contract{
		Name: "douyin-user", Platform: douyin.Platform, Category: "user", Version: "1",
		Transport: contracts.Transport{BaseURL: srvURL, Path: "/aweme/v1/web/user/profile/other/", Method: http.MethodGet},
		Binding:   contracts.Binding{Users: "$.user_list"},
	})
	return New(Context{
		Registry: reg, Obs: o,
		Names: map[string]map[string]string{douyin.Platform: {
			"comments": "douyin-comments", "user": "douyin-user",
		}},
	})
}

// TestCommentEnrichCombination: the payload + user-enrich combination fills
// the twelve-field profile — payload-bound values win (nickname, avatar,
// ip_label via the contract), enrich fills signature and the four counts,
// and one shared author costs exactly ONE enrich request.
func TestCommentEnrichCombination(t *testing.T) {
	fx := &enrichFixture{}
	fx.userMode.Store("ok")
	srv := fx.server(t)
	defer srv.Close()
	o := obs.NewCounterMap()
	e := enrichTestEngine(srv.URL, o)

	cmts, _, err := e.ItemComments(context.Background(), douyin.Platform, "7001", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("ItemComments: %v", err)
	}
	if len(cmts) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(cmts))
	}
	if n := atomic.LoadInt64(&fx.userReqs); n != 1 {
		t.Fatalf("expected exactly 1 enrich request for the shared author, got %d", n)
	}
	u := cmts[0].User
	// Payload wins.
	if u.Nickname != "payload-nick" || u.AvatarURL != "https://a/1.jpg" || u.IPLabel != "四川" {
		t.Fatalf("payload-bound fields must win, got %+v", u)
	}
	if u.ShortID != "11" {
		t.Fatalf("payload short_id must win, got %q", u.ShortID)
	}
	// Enrich fills the missing profile face fields.
	if u.Signature != "bio" || u.FollowerCount != 1200 || u.FollowingCount != 88 ||
		u.AwemeCount != 45 || u.TotalFavorited != 99000 {
		t.Fatalf("enrich must fill signature/counts, got %+v", u)
	}
	// Per-row backfill (final-audit P1): the SECOND row by the same author
	// must merge the same shared enrich result — it used to stay bare.
	u2 := cmts[1].User
	if u2.Nickname != "payload-nick" || u2.IPLabel != "四川" {
		t.Fatalf("row 2 payload-bound fields must win, got %+v", u2)
	}
	if u2.Signature != "bio" || u2.FollowerCount != 1200 || u2.FollowingCount != 88 ||
		u2.AwemeCount != 45 || u2.TotalFavorited != 99000 {
		t.Fatalf("row 2 must share the enrich pass result, got %+v", u2)
	}
	if got := o.Get("collect.comment_enrich"); got != 1 {
		t.Fatalf("obs collect.comment_enrich = %d, want 1", got)
	}
	if n := atomic.LoadInt64(&fx.userReqs); n != 1 {
		t.Fatalf("per-row backfill must not add requests, got %d", n)
	}
}

// TestCommentEnrichBestEffortAndCircuit: a dead enrich face never fails the
// comment collection; after three consecutive failures the circuit breaker
// stops further attempts.
func TestCommentEnrichBestEffortAndCircuit(t *testing.T) {
	fx := &enrichFixture{many: true}
	fx.userMode.Store("500")
	srv := fx.server(t)
	defer srv.Close()
	o := obs.NewCounterMap()
	e := enrichTestEngine(srv.URL, o)

	cmts, _, err := e.ItemComments(context.Background(), douyin.Platform, "7001", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("enrich failures must not fail the comment walk: %v", err)
	}
	if len(cmts) != 6 || cmts[0].User.Signature != "" || cmts[0].User.FollowerCount != 0 {
		t.Fatalf("comments must survive un-enriched: %+v", cmts[0].User)
	}
	if n := atomic.LoadInt64(&fx.userReqs); n > 3 {
		t.Fatalf("circuit breaker must stop after 3 attempts, got %d", n)
	}
	if got := o.Get("collect.comment_enrich_error"); got < 1 {
		t.Fatalf("obs collect.comment_enrich_error = %d, want >= 1", got)
	}
	if got := o.Get("collect.comment_enrich_circuit_open"); got != 1 {
		t.Fatalf("obs collect.comment_enrich_circuit_open = %d, want 1", got)
	}
}

// TestCommentEnrichKillSwitch: MEDIAMON_COMMENT_ENRICH=off issues zero enrich
// requests.
func TestCommentEnrichKillSwitch(t *testing.T) {
	t.Setenv("MEDIAMON_COMMENT_ENRICH", "off")
	fx := &enrichFixture{}
	fx.userMode.Store("ok")
	srv := fx.server(t)
	defer srv.Close()
	e := enrichTestEngine(srv.URL, obs.NewCounterMap())

	cmts, _, err := e.ItemComments(context.Background(), douyin.Platform, "7001", model.Cursor{}, 20)
	if err != nil || len(cmts) != 2 {
		t.Fatalf("comments walk: %v (%d)", err, len(cmts))
	}
	if n := atomic.LoadInt64(&fx.userReqs); n != 0 {
		t.Fatalf("kill switch must issue 0 enrich requests, got %d", n)
	}
	if cmts[0].User.FollowerCount != 0 {
		t.Fatalf("no enrich means no counts, got %d", cmts[0].User.FollowerCount)
	}
}

// TestGenderFromStringForms: kuaishou renders gender as "M"/"F" strings
// (corpus /rest/v/profile/get "sex":"M") — they must normalize to 1/2.
func TestGenderFromStringForms(t *testing.T) {
	cases := map[any]int{
		"M": 1, "m": 1, "male": 1, "男": 1,
		"F": 2, "f": 2, "female": 2, "女": 2,
		"garbage": 0, "": 0,
		float64(1): 1, float64(2): 2, float64(0): 0,
		"1": 1, "2": 2,
		nil: 0,
	}
	for in, want := range cases {
		if got := genderFrom(in); got != want {
			t.Fatalf("genderFrom(%v) = %d, want %d", in, got, want)
		}
	}
}

// TestMergeUserProfileFillForward: merge only fills empty/zero destinations.
func TestMergeUserProfileFillForward(t *testing.T) {
	dst := model.UserProfile{UID: "u1", Nickname: "kept", FollowerCount: 7}
	src := model.UserProfile{
		UID: "u2", SecUID: "s2", Nickname: "dropped", FollowerCount: 99,
		Signature: "bio", Gender: 2, Extra: map[string]any{"k": "v"},
	}
	mergeUserProfile(&dst, src)
	if dst.UID != "u1" || dst.Nickname != "kept" || dst.FollowerCount != 7 {
		t.Fatalf("non-empty fields must not be overwritten: %+v", dst)
	}
	if dst.SecUID != "s2" || dst.Signature != "bio" || dst.Gender != 2 || dst.Extra["k"] != "v" {
		t.Fatalf("empty fields must be filled: %+v", dst)
	}
}
