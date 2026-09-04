// commentthread_test.go — CommentThread unit tests: full chain walk,
// sub-closure accounting, CID-direct replies, commenter summary and the
// timeseries reference (earliest walked comment).
package collect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

func threadContracts(commentsBase, repliesBase string) []*contracts.Contract {
	return []*contracts.Contract{
		{
			Name: "mock-comments", Platform: "mock", Category: "comments", Version: "1",
			Transport: contracts.Transport{BaseURL: commentsBase, Path: "/c", Method: "GET", Placeholders: []string{"item_id"}},
			Binding: contracts.Binding{
				Comments: "$.comments",
				Fields: map[string]string{
					"reply_count":              "$.reply_comment_total",
					"extra.item_comment_total": "$.item_comment_total",
					"user.ip_label":            "$.ip_label",
				},
			},
			Paging: contracts.Paging{CursorParam: "cursor", CountParam: "count", CountDefault: 20, HasMorePath: "$.has_more", NextCursorPath: "$.cursor"},
		},
		{
			Name: "mock-comments-replies", Platform: "mock", Category: "replies", Version: "1",
			Transport: contracts.Transport{BaseURL: repliesBase, Path: "/r", Method: "GET", Placeholders: []string{"comment_id"}},
			Binding:   contracts.Binding{Comments: "$.comments"},
			Paging:    contracts.Paging{CursorParam: "cursor", CountParam: "count", CountDefault: 20, HasMorePath: "$.has_more", NextCursorPath: "$.cursor"},
		},
		{
			Name: "mock-user", Platform: "mock", Category: "user", Version: "1",
			Transport: contracts.Transport{BaseURL: "http://127.0.0.1:1", Path: "/u", Method: "GET", Placeholders: []string{"sec_uid"}},
			Binding:   contracts.Binding{Users: "$.user_list"},
		},
	}
}

func TestCommentThreadFullChainAndClosure(t *testing.T) {
	// Top-level: page1 = R1(3 claimed subs)+R2(0), page2 = R3(2 claimed).
	commentsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		w.Header().Set("Content-Type", "application/json")
		if cursor == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"comments": []map[string]any{
					{"cid": "R1", "text": "根1", "create_time": 1700000000, "reply_comment_total": 3, "item_comment_total": 5,
						"ip_label": "陕西", "user": map[string]any{"sec_uid": "SU1", "nickname": "评者一"}},
					{"cid": "R2", "text": "根2", "create_time": 1700003600, "reply_comment_total": 0, "item_comment_total": 5,
						"ip_label": "北京", "user": map[string]any{"sec_uid": "SU2", "nickname": "评者二"}},
				},
				"cursor": 20, "has_more": 1, "total": 5,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"comments": []map[string]any{
				{"cid": "R3", "text": "根3", "create_time": 1700007200, "reply_comment_total": 2, "item_comment_total": 5,
					"ip_label": "上海", "user": map[string]any{"sec_uid": "SU1", "nickname": "评者一"}},
			},
			"cursor": 40, "has_more": 0, "total": 5,
		})
	}))
	defer commentsSrv.Close()

	// Replies: R1 walks 2+1 across two pages (CID-direct, no item param);
	// R3 returns only 1 of its 2 claimed (closure gap by design here).
	repliesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("comment_id")
		cursor := r.URL.Query().Get("cursor")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case cid == "R1" && cursor == "":
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": []map[string]any{
				{"cid": "S1", "text": "子1", "create_time": 1700000600, "user": map[string]any{"sec_uid": "SU3", "nickname": "子者"}},
				{"cid": "S2", "text": "子2", "create_time": 1700001200, "user": map[string]any{"sec_uid": "SU3", "nickname": "子者"}},
			}, "cursor": 20, "has_more": 1})
		case cid == "R1":
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": []map[string]any{
				{"cid": "S3", "text": "子3", "create_time": 1700001800, "user": map[string]any{"sec_uid": "SU1", "nickname": "评者一"}},
			}, "cursor": 40, "has_more": 0})
		case cid == "R3":
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": []map[string]any{
				{"cid": "S4", "text": "子4", "create_time": 1700007300, "user": map[string]any{"sec_uid": "SU2", "nickname": "评者二"}},
			}, "cursor": 20, "has_more": 0})
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, "{}")
		}
	}))
	defer repliesSrv.Close()

	// Enrich face: answers every sec_uid with a full profile.
	userSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		su := r.URL.Query().Get("sec_uid")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_list": []map[string]any{{
				"sec_uid": su, "nickname": "评者一", "follower_count": 10, "aweme_count": 3,
			}},
		})
	}))
	defer userSrv.Close()

	cs := threadContracts(commentsSrv.URL, repliesSrv.URL)
	cs[2].Transport.BaseURL = userSrv.URL
	reg := addContracts(t, cs...)
	eng := mockEngine(t, reg, func(c *Context) {
		c.Pacing = &PacingConfig{}
		c.Names = map[string]map[string]string{"mock": {"replies": "mock-comments-replies"}}
	})

	out, err := eng.CommentThread(context.Background(), "mock", "ITEM1", CommentThreadOptions{})
	if err != nil {
		t.Fatalf("CommentThread: %v", err)
	}
	if out.NCommentsClaim != 5 {
		t.Fatalf("n_comments_claim = %d, want 5 (item_comment_total)", out.NCommentsClaim)
	}
	if out.NCommentsWalked != 3 || out.Pages != 2 {
		t.Fatalf("walked = %d pages = %d, want 3/2", out.NCommentsWalked, out.Pages)
	}
	if out.RootsWithSub != 2 {
		t.Fatalf("roots_with_sub = %d, want 2", out.RootsWithSub)
	}
	// claimed 3+2=5, walked 3+1=4 → closure 80%
	if out.SubClosure.Claimed != 5 || out.SubClosure.Walked != 4 || out.SubClosure.RootsWalked != 2 {
		t.Fatalf("sub closure wrong: %+v", out.SubClosure)
	}
	// commenters: SU1 (R1,S3? wait S3 user SU1 → R1,R3,S3 = 3), SU2 (R2,S4=2), SU3 (S1,S2=2)
	byKey := map[string]CommenterSummary{}
	for _, c := range out.Commenters {
		byKey[c.SecUID] = c
	}
	if len(byKey) != 3 {
		t.Fatalf("unique commenters = %d, want 3: %+v", len(byKey), out.Commenters)
	}
	if byKey["SU1"].NComments != 3 || byKey["SU2"].NComments != 2 || byKey["SU3"].NComments != 2 {
		t.Fatalf("commenter counts wrong: %+v", byKey)
	}
	if byKey["SU1"].FirstCommentTs != 1700000000 {
		t.Fatalf("first ts wrong: %+v", byKey["SU1"])
	}
	if byKey["SU1"].IPLabel != "陕西" {
		t.Fatalf("ip_label must ride the payload: %+v", byKey["SU1"])
	}
	// timeseries: base = earliest (1700000000); last = S4 (1700007300)
	if out.Timeseries.FirstDelayH != 0 {
		t.Fatalf("first delay = %v, want 0 (earliest is the base)", out.Timeseries.FirstDelayH)
	}
	if out.Timeseries.LastDelayH <= out.Timeseries.MedianDelayH || out.Timeseries.MedianDelayH < 0 {
		t.Fatalf("timeseries ordering wrong: %+v", out.Timeseries)
	}
	if !byKey["SU1"].ProfileHit || !byKey["SU3"].ProfileHit {
		t.Fatalf("enrich pass must mark profile hits: %+v", byKey)
	}
}

// TestCommentThreadSubRootLimit: the chain respects SubRootLimit.
func TestCommentThreadSubRootLimit(t *testing.T) {
	commentsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"comments": []map[string]any{
				{"cid": "R1", "create_time": 1, "reply_comment_total": 2, "item_comment_total": 9, "user": map[string]any{"sec_uid": "A"}},
				{"cid": "R2", "create_time": 2, "reply_comment_total": 2, "item_comment_total": 9, "user": map[string]any{"sec_uid": "B"}},
			},
			"cursor": 20, "has_more": 0,
		})
	}))
	defer commentsSrv.Close()
	var replyCalls int
	repliesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replyCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"comments": []map[string]any{
			{"cid": "S", "create_time": 3, "user": map[string]any{"sec_uid": "C"}},
		}, "cursor": 20, "has_more": 0})
	}))
	defer repliesSrv.Close()
	cs := threadContracts(commentsSrv.URL, repliesSrv.URL)
	reg := addContracts(t, cs...)
	eng := mockEngine(t, reg, func(c *Context) {
		c.Pacing = &PacingConfig{}
		c.Names = map[string]map[string]string{"mock": {"replies": "mock-comments-replies"}}
	})
	out, err := eng.CommentThread(context.Background(), "mock", "I", CommentThreadOptions{SubRootLimit: 1})
	if err != nil {
		t.Fatalf("CommentThread: %v", err)
	}
	if out.SubClosure.RootsWalked != 1 || replyCalls != 1 {
		t.Fatalf("sub root cap wrong: %+v calls=%d", out.SubClosure, replyCalls)
	}
}
