// suggest_test.go — SuggestWords unit tests: dy group form (nested words +
// source attribution + challenge_id), dy inbox/hot form, xhs word form.
package collect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

func suggestEngine(t *testing.T, c *contracts.Contract, handler http.HandlerFunc) *Engine {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c.Transport.BaseURL = srv.URL
	return mockEngine(t, addContracts(t, c), func(ctx *Context) {
		ctx.Pacing = &PacingConfig{}
		ctx.Names = map[string]map[string]string{c.Platform: {"suggest": c.Name}}
	})
}

func TestSuggestWordsDouyinGroupForm(t *testing.T) {
	c := &contracts.Contract{
		Name: "mock-suggest", Platform: "mock", Category: "suggest", Version: "1",
		Transport: contracts.Transport{Path: "/suggest", Method: "GET", Query: map[string]string{"query": ""}},
		Binding: contracts.Binding{
			Items:  "$.data",
			Fields: map[string]string{"extra.source": "$.source"},
		},
	}
	var gotQuery string
	eng := suggestEngine(t, c, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		words := make([]map[string]any, 0, 10)
		for i := 0; i < 10; i++ {
			words = append(words, map[string]any{
				"id": "1000000000000000000",
				"params": map[string]any{
					"challenge_id": "0",
					"extra_info":   map[string]any{"sentence_id": "0"},
				},
				"word": "滑板教学",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"source": "related_search", "type": "related_search",
				"params": map[string]any{"channel_id": "94349538563"},
				"words":  words,
			}},
			"errno": "0", "msg": "success",
		})
	})
	res, err := eng.SuggestWords(context.Background(), "mock", "滑板教学")
	if err != nil {
		t.Fatalf("SuggestWords: %v", err)
	}
	if gotQuery != "滑板教学" {
		t.Fatalf("query slot param = %q, want the contract's empty static key", gotQuery)
	}
	if res.Source != "related_search" {
		t.Fatalf("source = %q", res.Source)
	}
	if len(res.Words) != 10 {
		t.Fatalf("words = %d, want 10", len(res.Words))
	}
	if res.Words[0].Word != "滑板教学" || res.Words[0].ChallengeID != "0" || res.Words[0].ID == "" {
		t.Fatalf("word binding wrong: %+v", res.Words[0])
	}
	if res.Words[0].Category != "related_search" {
		t.Fatalf("category attribution wrong: %+v", res.Words[0])
	}
	if len(res.HotWords) != 0 {
		t.Fatalf("related_search form must not fill hot_words: %+v", res.HotWords)
	}
}

func TestSuggestWordsDouyinInboxHotForm(t *testing.T) {
	c := &contracts.Contract{
		Name: "mock-suggest", Platform: "mock", Category: "suggest", Version: "1",
		Transport: contracts.Transport{Path: "/suggest", Method: "GET", Query: map[string]string{"query": ""}},
		Binding:   contracts.Binding{Items: "$.data", Fields: map[string]string{"extra.source": "$.source"}},
	}
	eng := suggestEngine(t, c, func(w http.ResponseWriter, r *http.Request) {
		words := []map[string]any{}
		for _, w := range []string{"热词一", "热词二", "热词三"} {
			words = append(words, map[string]any{"id": "1", "word": w})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"source": "inbox", "type": "inbox", "words": words}},
		})
	})
	res, err := eng.SuggestWords(context.Background(), "mock", "")
	if err != nil {
		t.Fatalf("SuggestWords: %v", err)
	}
	if res.Source != "inbox" || len(res.Words) != 3 {
		t.Fatalf("inbox form wrong: %+v", res)
	}
	if len(res.HotWords) != 3 || res.HotWords[0] != "热词一" {
		t.Fatalf("hot words mirror wrong: %+v", res.HotWords)
	}
}

func TestSuggestWordsXhsWordForm(t *testing.T) {
	c := &contracts.Contract{
		Name: "mock-suggest-xhs", Platform: "mockx", Category: "suggest", Version: "1",
		Transport: contracts.Transport{Path: "/rec", Method: "GET", Query: map[string]string{"keyword": ""}},
		Binding: contracts.Binding{
			Items:  "$.data.sug_items",
			Fields: map[string]string{"word": "$.text", "category": "$.type"},
		},
	}
	var gotKeyword string
	eng := suggestEngine(t, c, func(w http.ResponseWriter, r *http.Request) {
		gotKeyword = r.URL.Query().Get("keyword")
		items := []map[string]any{}
		for i := 0; i < 10; i++ {
			items = append(items, map[string]any{
				"highlight_flags": []bool{true, true, false}, "search_type": "notes",
				"type": "top_note", "text": "美食探店",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "code": 1000,
			"data": map[string]any{"sug_items": items, "word_request_id": "r#1"},
		})
	})
	res, err := eng.SuggestWords(context.Background(), "mockx", "美食")
	if err != nil {
		t.Fatalf("SuggestWords: %v", err)
	}
	if gotKeyword != "美食" {
		t.Fatalf("keyword slot = %q, want 美食", gotKeyword)
	}
	if res.Source != "recommend" || len(res.Words) != 10 {
		t.Fatalf("xhs form wrong: %+v", res)
	}
	if res.Words[0].Word != "美食探店" || res.Words[0].Category != "top_note" {
		t.Fatalf("word/category wrong: %+v", res.Words[0])
	}
}

func TestSuggestWordsNotDeclared(t *testing.T) {
	reg := addContracts(t, &contracts.Contract{
		Name: "mock-user", Platform: "mock", Category: "user", Version: "1",
		Transport: contracts.Transport{BaseURL: "http://127.0.0.1:1", Path: "/u", Method: "GET"},
		Binding:   contracts.Binding{Users: "$.user_list"},
	})
	eng := mockEngine(t, reg, nil)
	if _, err := eng.SuggestWords(context.Background(), "mock", "q"); err == nil {
		t.Fatal("want fail-closed error without suggest contract")
	}
}
