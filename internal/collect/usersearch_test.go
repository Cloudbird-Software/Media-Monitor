// usersearch_test.go — UserSearch unit tests: pcursor rewind termination
// (the "1" loop), extra-field binding and the paced profile join.
package collect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

func userSearchContract(base string) *contracts.Contract {
	return &contracts.Contract{
		Name: "mock-user-search", Platform: "mock", Category: "user_search", Version: "1",
		Transport: contracts.Transport{
			BaseURL: base, Path: "/rest/v/search/user", Method: "POST",
			Query: map[string]string{"keyword": ""},
			Body:  map[string]any{"kpn": "PC_WEB"},
		},
		Binding: contracts.Binding{
			Users: "$.users",
			Fields: map[string]string{
				"avatar_url":         "$.headurl",
				"signature":          "$.user_text",
				"extra.verified":     "$.verified",
				"extra.living":       "$.livingInfo.living",
				"extra.is_following": "$.isFollowing",
			},
		},
		Paging: contracts.Paging{
			CursorParam:    "pcursor",
			HasMorePath:    "$.pcursor",
			NextCursorPath: "$.pcursor",
		},
	}
}

// TestUserSearchPcursorRewindStops: the corpus cursor rewinds to "1" and
// re-serves the same window; the walk must stop on the no-new-users page.
func TestUserSearchPcursorRewindStops(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Same 30-shaped window on every page (corpus rewind shape).
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": 1, "pcursor": "1", "searchSessionId": "s-1",
			"users": []map[string]any{
				{"user_id": "3xa", "user_name": "用户甲", "headurl": "http://h/1", "verified": true,
					"isFollowing": false, "livingInfo": map[string]any{"living": true},
					"user_text": "更新生活"},
				{"user_id": "3xb", "user_name": "用户乙", "headurl": "http://h/2", "verified": false,
					"isFollowing": true, "livingInfo": map[string]any{"living": false},
					"user_text": "更新美食"},
			},
		})
	}))
	defer srv.Close()
	reg := addContracts(t, userSearchContract(srv.URL))
	eng := mockEngine(t, reg, func(c *Context) { c.Pacing = &PacingConfig{} })

	users, cur, err := eng.UserSearch(context.Background(), "mock", "露营", model.Cursor{}, 0, UserSearchOptions{})
	if err != nil {
		t.Fatalf("UserSearch: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("unique users = %d, want 2 (rewind dedup)", len(users))
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("page fetches = %d, want 2 (page 2 has no new users → stop)", got)
	}
	if !cur.HasMore {
		t.Fatalf("cursor should surface the live rewind state: %+v", cur)
	}
	u := users[0]
	if u.User.UID != "3xa" || u.User.Nickname != "用户甲" || u.User.AvatarURL != "http://h/1" {
		t.Fatalf("user binding wrong: %+v", u.User)
	}
	if !u.Verified || !u.Living {
		t.Fatalf("verified/living extra fields wrong: %+v", u)
	}
	if users[1].Verified || users[1].Living {
		t.Fatalf("second user extras wrong: %+v", users[1])
	}
}

// TestUserSearchProfileJoin: JoinProfiles merges the platform user face
// fill-forward and marks the join hit.
func TestUserSearchProfileJoin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": 1, "pcursor": "no_more",
			"users": []map[string]any{
				{"user_id": "3xa", "user_name": "用户甲", "headurl": "", "verified": false,
					"livingInfo": map[string]any{"living": false}, "user_text": ""},
			},
		})
	}))
	defer srv.Close()
	userFace := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": 1,
			"user_list": []map[string]any{{
				"user_id": "3xa", "nickname": "用户甲", "fans": 50958, "follower_count": 50958,
				"follows": 193, "following_count": 193, "aweme_count": 18,
			}},
		})
	}))
	defer userFace.Close()
	reg := addContracts(t,
		userSearchContract(srv.URL),
		&contracts.Contract{
			Name: "mock-user", Platform: "mock", Category: "user", Version: "1",
			Transport: contracts.Transport{BaseURL: userFace.URL, Path: "/u", Method: "GET", Placeholders: []string{"sec_uid"}},
			Binding:   contracts.Binding{Users: "$.user_list"},
		},
	)
	eng := mockEngine(t, reg, func(c *Context) { c.Pacing = &PacingConfig{} })
	users, _, err := eng.UserSearch(context.Background(), "mock", "露营", model.Cursor{}, 0,
		UserSearchOptions{JoinProfiles: true, JoinLimit: 5})
	if err != nil {
		t.Fatalf("UserSearch: %v", err)
	}
	if len(users) != 1 || !users[0].ProfileHit {
		t.Fatalf("join must hit: %+v", users)
	}
	if users[0].User.FollowerCount != 50958 || users[0].User.AwemeCount != 18 {
		t.Fatalf("join must fill counts: %+v", users[0].User)
	}
	// nickname consistency (search user_name == profile nickname)
	if users[0].User.Nickname != "用户甲" {
		t.Fatalf("nickname join drift: %+v", users[0].User)
	}
}

// TestUserSearchNotDeclared: platforms without a user_search contract
// fail closed.
func TestUserSearchNotDeclared(t *testing.T) {
	reg := addContracts(t, &contracts.Contract{
		Name: "mock-user", Platform: "mock", Category: "user", Version: "1",
		Transport: contracts.Transport{BaseURL: "http://127.0.0.1:1", Path: "/u", Method: "GET"},
		Binding:   contracts.Binding{Users: "$.user_list"},
	})
	eng := mockEngine(t, reg, nil)
	if _, _, err := eng.UserSearch(context.Background(), "mock", "kw", model.Cursor{}, 0, UserSearchOptions{}); err == nil {
		t.Fatal("want fail-closed error without user_search contract")
	}
}
