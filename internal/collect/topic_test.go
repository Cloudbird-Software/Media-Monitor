// topic_test.go — TopicFeed unit tests: dy text_extra anchor face (declared
// contract paths, parallel hashtag ids), ks feed-tags face, absent face
// (xhs-shaped records), '#' normalization, exact-match anchoring, shape-
// default fallback, fail-closed rows.
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

// dyTopicContract mirrors the douyin-search shape: cards under $.data with
// aweme_info + declared topic anchor fields.
func dyTopicContract(srv *httptest.Server) *contracts.Contract {
	return &contracts.Contract{
		Name: "mock-dy-search", Platform: "mockdy", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/search/single", Method: http.MethodGet},
		Binding: contracts.Binding{
			Items: "$.data",
			Fields: map[string]string{
				"topic.tags":   "$.aweme_info.text_extra[*].hashtag_name",
				"topic.tag_id": "$.aweme_info.text_extra[*].hashtag_id",
			},
		},
		Paging: contracts.Paging{
			CursorParam: "offset", CountParam: "count", CountDefault: 20,
			HasMorePath: "$.has_more", NextCursorPath: "$.cursor",
		},
	}
}

// ksTopicContract mirrors the kuaishou-search shape: feeds with record-level tags.
func ksTopicContract(srv *httptest.Server) *contracts.Contract {
	return &contracts.Contract{
		Name: "mock-ks-search", Platform: "mockks", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/search/feed", Method: http.MethodGet},
		Binding: contracts.Binding{
			Items:  "$.feeds",
			Fields: map[string]string{"topic.tags": "$.tags[*].name"},
		},
		Paging: contracts.Paging{
			CursorParam: "pcursor", CountParam: "count", CountDefault: 20,
			HasMorePath: "$.pcursor", NextCursorPath: "$.pcursor",
		},
	}
}

func topicEngine(t *testing.T, c *contracts.Contract) *Engine {
	t.Helper()
	return mockEngine(t, addContracts(t, c), func(ctx *Context) {
		ctx.Pacing = &PacingConfig{}
		ctx.Names = map[string]map[string]string{c.Platform: {"search": c.Name}}
	})
}

func TestTopicFeedDouyinTextExtraAnchors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 20 cards, every 4th carrying the exact topic anchor with its id.
		cards := []map[string]any{}
		for i := 0; i < 20; i++ {
			te := []map[string]any{
				{"hashtag_name": "川西自驾", "hashtag_id": "1111111111111111111"},
			}
			if i%4 == 0 {
				te = append(te, map[string]any{"hashtag_name": "露营装备", "hashtag_id": "7669252810335219619"})
			}
			cards = append(cards, map[string]any{
				"aweme_info": map[string]any{
					"aweme_id":   fmt.Sprintf("7700000000000000%03d", i),
					"desc":       "test",
					"text_extra": te,
					"statistics": map[string]any{"digg_count": 10},
				},
			})
		}
		_ = writeJSON(w, map[string]any{"data": cards, "has_more": false, "cursor": 20})
	}))
	defer srv.Close()
	eng := topicEngine(t, dyTopicContract(srv))
	res, err := eng.TopicFeed(context.Background(), "mockdy", "#露营装备", TopicOptions{})
	if err != nil {
		t.Fatalf("TopicFeed: %v", err)
	}
	if len(res.Items) != 20 || res.AnchoredItems != 5 {
		t.Fatalf("anchoring wrong: items=%d anchored=%d, want 20/5", len(res.Items), res.AnchoredItems)
	}
	if res.Meta.AnchorFace != "contract" {
		t.Fatalf("anchor face = %q, want contract (declared topic.tags)", res.Meta.AnchorFace)
	}
	if res.Meta.HashtagID != "7669252810335219619" {
		t.Fatalf("hashtag id = %q, want the topic's parallel-array id", res.Meta.HashtagID)
	}
	if res.Topic != "露营装备" {
		t.Fatalf("topic normalization wrong: %q", res.Topic)
	}
}

func TestTopicFeedKuaishouTagsAndDefaultsFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feeds := []map[string]any{}
		for i := 0; i < 20; i++ {
			tags := []map[string]any{{"name": "磁力万象", "type": 1}}
			if i%5 == 0 {
				tags = append(tags, map[string]any{"name": "快手小剧场", "type": 1})
			}
			feeds = append(feeds, map[string]any{
				"photo": map[string]any{"id": fmt.Sprintf("5100000000000000%03d", i), "caption": "c"},
				"tags":  tags,
			})
		}
		_ = writeJSON(w, map[string]any{"feeds": feeds, "pcursor": "no_more"})
	}))
	defer srv.Close()
	eng := topicEngine(t, ksTopicContract(srv))
	res, err := eng.TopicFeed(context.Background(), "mockks", "快手小剧场", TopicOptions{})
	if err != nil {
		t.Fatalf("TopicFeed: %v", err)
	}
	if len(res.Items) != 20 || res.AnchoredItems != 4 {
		t.Fatalf("ks anchoring wrong: items=%d anchored=%d, want 20/4", len(res.Items), res.AnchoredItems)
	}
	if res.Meta.AnchorFace != "contract" || res.Meta.HashtagID != "" {
		t.Fatalf("ks meta wrong: %+v (tags face carries no ids)", res.Meta)
	}
	// Shape-default fallback: a contract WITHOUT declared topic fields still
	// anchors through the tags[*].name family.
	bare := ksTopicContract(srv)
	bare.Name = "mock-ks-bare"
	bare.Binding.Fields = map[string]string{}
	eng2 := topicEngine(t, bare)
	res2, err := eng2.TopicFeed(context.Background(), "mockks", "快手小剧场", TopicOptions{})
	if err != nil {
		t.Fatalf("TopicFeed(bare): %v", err)
	}
	if res2.AnchoredItems != 4 || res2.Meta.AnchorFace != "tags" {
		t.Fatalf("shape-default fallback wrong: %+v", res2.Meta)
	}
}

func TestTopicFeedAbsentFaceStillListsContent(t *testing.T) {
	// xhs-shaped records: note cards without any tag structure — the anchor
	// face is absent (AnchoredItems=0, face "") but the content list still
	// collects (and id-less hot_query cards are skipped).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		items := []map[string]any{}
		for i := 0; i < 20; i++ {
			items = append(items, map[string]any{
				"id":         fmt.Sprintf("65f0000000000000%03d", i),
				"model_type": "note",
				"note_card": map[string]any{
					"display_title": "露营笔记", "type": "normal",
					"interact_info": map[string]any{"liked_count": "12"},
				},
			})
		}
		items = append(items, map[string]any{"model_type": "hot_query", "hot_query": map[string]any{}})
		_ = writeJSON(w, map[string]any{"data": map[string]any{"items": items, "has_more": false}})
	}))
	defer srv.Close()
	c := &contracts.Contract{
		Name: "mock-xhs-search", Platform: "mockxhs", Category: "search", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/search/notes", Method: http.MethodGet},
		Binding:   contracts.Binding{Items: "$.data.items"},
	}
	eng := topicEngine(t, c)
	res, err := eng.TopicFeed(context.Background(), "mockxhs", "露营", TopicOptions{})
	if err != nil {
		t.Fatalf("TopicFeed: %v", err)
	}
	if len(res.Items) != 20 {
		t.Fatalf("content list wrong: %d items (hot_query card must be skipped)", len(res.Items))
	}
	if res.AnchoredItems != 0 || res.Meta.AnchorFace != "" || res.Meta.HashtagID != "" {
		t.Fatalf("absent face must report empty metadata: %+v", res.Meta)
	}
}

func TestTopicFeedFailClosedAndEmptyTopic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = writeJSON(w, map[string]any{"data": []any{}, "has_more": false})
	}))
	defer srv.Close()
	eng := topicEngine(t, dyTopicContract(srv))
	if _, err := eng.TopicFeed(context.Background(), "mockdy", "  # ", TopicOptions{}); err == nil || !strings.Contains(err.Error(), "empty topic") {
		t.Fatalf("empty topic must fail closed, got %v", err)
	}
	if _, err := eng.TopicFeed(context.Background(), "nosuch", "露营", TopicOptions{}); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared search contract must fail closed, got %v", err)
	}
}
