package collect

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

// TestUserPostsEngineWalk: the engine atom walks the real douyin-user-posts
// fixtures newest-first with the backtrack predicate wired in (low items
// stop after N) and hands the caller a resumable cursor.
func TestUserPostsEngineWalk(t *testing.T) {
	srv, _ := userPostsFixtureServer(t)
	defer srv.Close()
	reg := testkit.RemapContracts(t, testkit.ContractsDir(t, 2), srv, "douyin-user-posts")
	eng := New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"ua"}}),
		Obs:      obs.NewCounterMap(),
		Signers: map[string]httpclient.Signer{"douyin": httpclient.StaticSigner{
			Fn: func(_ context.Context, _ string, _ string, _ map[string]string) (map[string]string, error) {
				return map[string]string{"a_bogus": "x"}, nil
			},
		}},
		Cookies: map[string]string{"douyin": "ttwid=t"},
	})
	// fixtures carry 2 items/page; with a floor of 1000 digs, page 2's first
	// item (2100 digs) qualifies high, second is high — walk reaches the
	// terminal third page without early stop.
	items, cur, err := eng.UserPosts(context.Background(), "douyin", "MS4wLjABAAAA-example-creator-0000000001", model.Cursor{}, 0, BacktrackOptions{
		MinEngagement: &EngagementFloor{Metric: "digg", Threshold: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 6 {
		t.Fatalf("items = %d, want 6", len(items))
	}
	for i, it := range items {
		if it.Stats.Digg == 0 || it.CreateTime == 0 {
			t.Fatalf("item %d missing stats/create_time", i)
		}
	}
	if cur.HasMore {
		t.Fatal("terminal page must end has_more=false")
	}
}

// TestUserPostsUndeclaredPlatform: kuaishou declares no user_posts contract
// — the engine fails closed with the explicit not-declared error.
func TestUserPostsUndeclaredPlatform(t *testing.T) {
	reg := contracts.NewRegistry()
	eng := New(Context{Registry: reg, HTTP: httpclient.New(httpclient.Config{}), Obs: obs.NewCounterMap()})
	_, _, err := eng.UserPosts(context.Background(), "kuaishou", "s", model.Cursor{}, 0, BacktrackOptions{})
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("err = %v, want explicit not-declared", err)
	}
}
