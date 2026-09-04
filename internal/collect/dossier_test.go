// dossier_test.go — AuthorDossier unit tests (in-process mock faces):
// dedup termination on a regressing cursor, claimed-face binding and the
// claimed-vs-observed consistency math.
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

// mockProfileFace serves one user_list record for any sec_uid.
func mockProfileFace(t *testing.T, nickname string, followers, favorited, awemes int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_list": []map[string]any{{
				"uid": "u-1", "sec_uid": r.URL.Query().Get("sec_uid"),
				"nickname": nickname, "signature": "bio",
				"follower_count": followers, "total_favorited": favorited,
				"aweme_count": awemes, "following_count": 7,
			}},
		})
	}))
}

// TestAuthorDossierDedupTermination: a user_posts face whose cursor
// REGRESSES (returns the same window with has_more=1 forever) must
// terminate through id dedup + stop-on-no-new-page, not the page guard.
func TestAuthorDossierDedupTermination(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cur := r.URL.Query().Get("max_cursor")
		// Page 1: two works, cursor advances; page 2: one new + the same
		// two (regressed window); page 3: only already-seen records.
		var page []map[string]any
		switch cur {
		case "":
			page = []map[string]any{
				{"aweme_id": "w1", "create_time": 1700000000, "statistics": map[string]any{"digg_count": 100}},
				{"aweme_id": "w2", "create_time": 1700008600, "statistics": map[string]any{"digg_count": 50}},
			}
		case "900":
			page = []map[string]any{
				{"aweme_id": "w2", "create_time": 1700008600, "statistics": map[string]any{"digg_count": 50}}, // dup
				{"aweme_id": "w3", "create_time": 1700017200, "statistics": map[string]any{"digg_count": 10}},
			}
		default:
			page = []map[string]any{
				{"aweme_id": "w1", "create_time": 1700000000, "statistics": map[string]any{"digg_count": 100}}, // dup
				{"aweme_id": "w2", "create_time": 1700008600, "statistics": map[string]any{"digg_count": 50}},  // dup
			}
		}
		next := "900"
		if cur == "900" {
			next = "800"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"aweme_list": page, "max_cursor": next, "has_more": 1,
		})
	}))
	defer srv.Close()

	prof := mockProfileFace(t, "作者甲", 1200, 99000, 5)
	defer prof.Close()

	reg := addContracts(t,
		&contracts.Contract{
			Name: "mock-user-posts", Platform: "mock", Category: "user_posts", Version: "1",
			Transport: contracts.Transport{BaseURL: srv.URL, Path: "/post", Method: "GET", Placeholders: []string{"sec_uid"}},
			Binding:   contracts.Binding{Items: "$.aweme_list"},
			Paging:    contracts.Paging{CursorParam: "max_cursor", CountParam: "count", CountDefault: 20, HasMorePath: "$.has_more", NextCursorPath: "$.max_cursor"},
		},
		&contracts.Contract{
			Name: "mock-user", Platform: "mock", Category: "user", Version: "1",
			Transport: contracts.Transport{BaseURL: prof.URL, Path: "/u", Method: "GET", Placeholders: []string{"sec_uid"}},
			Binding:   contracts.Binding{Users: "$.user_list"},
		},
	)
	eng := mockEngine(t, reg, func(c *Context) { c.Pacing = &PacingConfig{} })

	d, err := eng.AuthorDossier(context.Background(), "mock", "SEC1", DossierOptions{})
	if err != nil {
		t.Fatalf("AuthorDossier: %v", err)
	}
	if d.Observed.WorksUnique != 3 {
		t.Fatalf("works_unique = %d, want 3 (dedup by aweme_id)", d.Observed.WorksUnique)
	}
	if d.Observed.WorksWalked != 6 {
		t.Fatalf("works_walked = %d, want 6 records on the wire", d.Observed.WorksWalked)
	}
	if d.Observed.Pages != 3 {
		t.Fatalf("pages = %d, want 3 (third page all-dup stops the walk)", d.Observed.Pages)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("page fetches = %d, want 3", got)
	}
	if d.Observed.SumDigg != 160 || d.Observed.MaxDigg != 100 || d.Observed.MedianDigg != 50 {
		t.Fatalf("digg stats wrong: %+v", d.Observed)
	}
	// interval w2-w1 = w3-w2 = 8600s → 0.0995… days; span = 17200s.
	if d.Observed.MedianIntervalDays <= 0 || d.Observed.PublishSpanDays <= 0 {
		t.Fatalf("rhythm stats wrong: %+v", d.Observed)
	}
	if d.Claimed.Nickname != "作者甲" || d.Claimed.FollowerCount != 1200 || d.Claimed.AwemeCount != 5 {
		t.Fatalf("claimed face wrong: %+v", d.Claimed)
	}
	if want := int64(5 - 3); d.Consistency.CountDelta != want {
		t.Fatalf("count_delta = %d, want %d", d.Consistency.CountDelta, want)
	}
	if d.Consistency.FavoritedVsSumDiggRatio <= 0 {
		t.Fatalf("favorited ratio = %v, want > 0", d.Consistency.FavoritedVsSumDiggRatio)
	}
}

// TestAuthorDossierProfileCategoryWins: when the platform declares a
// "profile" contract, its values are primary and the user face only fills
// forward the gaps.
func TestAuthorDossierProfileCategoryWins(t *testing.T) {
	profFace := mockProfileFace(t, "画像面", 500, 8000, 9)
	defer profFace.Close()
	userFace := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_list": []map[string]any{{
				"sec_uid": "SEC9", "nickname": "用户面", "follower_count": 1,
				"ip_label": "上海", "aweme_count": 2,
			}},
		})
	}))
	defer userFace.Close()
	posts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"aweme_list": []map[string]any{{"aweme_id": "x1", "create_time": 1700000100}},
			"max_cursor": 0, "has_more": 0,
		})
	}))
	defer posts.Close()

	reg := addContracts(t,
		&contracts.Contract{
			Name: "mock-profile", Platform: "mock", Category: "profile", Version: "1",
			Transport: contracts.Transport{BaseURL: profFace.URL, Path: "/p", Method: "GET", Placeholders: []string{"sec_uid"}},
			Binding:   contracts.Binding{Users: "$.user_list"},
		},
		&contracts.Contract{
			Name: "mock-user", Platform: "mock", Category: "user", Version: "1",
			Transport: contracts.Transport{BaseURL: userFace.URL, Path: "/u", Method: "GET", Placeholders: []string{"sec_uid"}},
			Binding:   contracts.Binding{Users: "$.user_list"},
		},
		&contracts.Contract{
			Name: "mock-user-posts", Platform: "mock", Category: "user_posts", Version: "1",
			Transport: contracts.Transport{BaseURL: posts.URL, Path: "/post", Method: "GET", Placeholders: []string{"sec_uid"}},
			Binding:   contracts.Binding{Items: "$.aweme_list"},
			Paging:    contracts.Paging{HasMorePath: "$.has_more", NextCursorPath: "$.max_cursor"},
		},
	)
	eng := mockEngine(t, reg, func(c *Context) { c.Pacing = &PacingConfig{} })
	d, err := eng.AuthorDossier(context.Background(), "mock", "SEC9", DossierOptions{})
	if err != nil {
		t.Fatalf("AuthorDossier: %v", err)
	}
	if d.Claimed.Nickname != "画像面" || d.Claimed.FollowerCount != 500 || d.Claimed.AwemeCount != 9 {
		t.Fatalf("profile face must win: %+v", d.Claimed)
	}
	if d.Profile.IPLabel != "上海" {
		t.Fatalf("user face must fill forward ip_label: %+v", d.Profile)
	}
	if d.Observed.WorksUnique != 1 {
		t.Fatalf("works_unique = %d, want 1", d.Observed.WorksUnique)
	}
}

// TestAuthorDossierNoPostsContract: fail-closed when the platform declares
// no user_posts face (e.g. ks before kuaishou-profile-feed).
func TestAuthorDossierNoPostsContract(t *testing.T) {
	reg := addContracts(t, &contracts.Contract{
		Name: "mock-user", Platform: "mock", Category: "user", Version: "1",
		Transport: contracts.Transport{BaseURL: "http://127.0.0.1:1", Path: "/u", Method: "GET"},
		Binding:   contracts.Binding{Users: "$.user_list"},
	})
	eng := mockEngine(t, reg, nil)
	if _, err := eng.AuthorDossier(context.Background(), "mock", "S", DossierOptions{}); err == nil {
		t.Fatal("want error when user_posts contract is not declared")
	}
	// and the cursor type sanity: model.Cursor zero value stays resumable
	_ = model.Cursor{}
}
