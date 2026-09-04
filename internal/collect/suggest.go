// suggest.go — SuggestWords, the search-suggestion/hot-word collection
// atom (capability proposal D, P1): given a query/prefix (or none for the
// inbox/hot form), produce the suggestion word list with per-word category
// attribution — the recall-expansion infrastructure for keyword monitoring.
//
// Design notes (atom boundary / schema trade-offs):
//   - Two corpus shapes share one atom: dy suggest_words returns a list of
//     GROUPS (each {source, type, params.channel_id, words[10x]}), xhs
//     search/recommend returns the word items directly
//     ({text, type, search_type, highlight_flags}). The contract's items
//     binding selects the list root (data vs data.sug_items); the group vs
//     word distinction is detected from the record shape (a "words" array
//     = group form), with the same contract-fields-override discipline the
//     item/comment binders use for key families.
//   - The xhs search-response hot_query card (data.items[].hot_query) is a
//     SEARCH-face artifact, not a suggest face: collecting it would walk
//     search pages, so it stays out of this atom (documented exclusion;
//     the atom's hot words come from the platform's own inbox/hot form).
//   - Word-record key families (word|text, id, params.challenge_id) are
//     shape defaults overridable per contract through binding.fields
//     ("word", "word_id", "challenge_id").
package collect

import (
	"context"
	"fmt"
	"sort"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// SuggestWord is one suggestion word with its category attribution.
type SuggestWord struct {
	Word        string `json:"word"`
	ID          string `json:"id,omitempty"`
	ChallengeID string `json:"challenge_id,omitempty"`
	// Category is the platform's own attribution slot: dy group source/type
	// (related_search|inbox), xhs per-item type/search_type (top_note/notes).
	Category string `json:"category,omitempty"`
}

// SuggestResult is the collected suggestion ecosystem for one query.
type SuggestResult struct {
	Site  string `json:"site"`
	Query string `json:"query"`
	// Source names the form that produced the words: related_search |
	// inbox (dy), recommend (xhs), hot (empty-query fallbacks).
	Source   string        `json:"source"`
	Words    []SuggestWord `json:"words"`
	HotWords []string      `json:"hot_words,omitempty"` // inbox/hot form mirror
}

// suggestWordKeyFamilies are the default record keys of a word item.
var suggestWordKeyFamilies = map[string][]string{
	"word":         {"word", "text"},
	"word_id":      {"id", "word_id"},
	"challenge_id": {"params.challenge_id", "challenge_id"},
}

// querySlotParam resolves the caller's query-slot parameter NAME from the
// contract (data, not platform code): the transport.query key declared
// with an empty static value is the slot (dy "query", xhs "keyword",
// ks user-search "keyword"). Falls back to "query". The empty static value
// also keeps the key on the wire for the inbox/hot form, matching the
// synth oracle's tolerant face; a strict real-site adaptation drops it in
// a version bump.
func querySlotParam(c *contracts.Contract) string {
	var empties []string
	for k, v := range c.Transport.Query {
		if v == "" {
			empties = append(empties, k)
		}
	}
	if len(empties) == 0 {
		return "query"
	}
	sort.Strings(empties)
	return empties[0]
}

// SuggestWords collects the suggestion words for a query prefix. An empty
// query fetches the platform's inbox/hot form (dy: 9 hot words; xhs: the
// static discovery pool).
func (e *Engine) SuggestWords(ctx context.Context, platform, query string) (SuggestResult, error) {
	var res SuggestResult
	res.Site = platform
	res.Query = query
	name, err := e.resolveName(platform, "suggest")
	if err != nil {
		return res, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return res, fmt.Errorf("collect: contract %q not registered", name)
	}
	var pathParams map[string]string
	if query != "" {
		pathParams = map[string]string{querySlotParam(c): query}
	}
	doc, err := e.Fetch(ctx, name, pathParams, nil)
	if err != nil {
		return res, err
	}
	_, raw := mainBindingRaw(c)
	if raw == "" {
		return res, fmt.Errorf("collect %s: no list binding declared for suggest words", name)
	}
	bp, err := contracts.ParsePath(raw)
	if err != nil {
		return res, err
	}
	for _, rec := range selectRecords(bp, doc) {
		if nested := suggestNestedWords(c, rec); nested != nil {
			// Group form (dy): source/type ride the group, words inside.
			src := fieldStr(c, "extra.source", rec, []string{"source", "type"})
			if res.Source == "" {
				res.Source = src
			}
			for _, w := range nested {
				w.Category = pickNonEmpty(w.Category, src)
				res.Words = append(res.Words, w)
			}
			continue
		}
		// Word form (xhs): one word per record.
		w := SuggestWord{
			Word:     fieldStr(c, "word", rec, suggestWordKeyFamilies["word"]),
			ID:       fieldStr(c, "word_id", rec, suggestWordKeyFamilies["word_id"]),
			Category: fieldStr(c, "category", rec, []string{"type", "search_type"}),
		}
		if v := resolveValue(c, "", rec, suggestWordKeyFamilies["challenge_id"]); v != nil {
			w.ChallengeID = asStr(v)
		}
		if w.Word == "" {
			continue
		}
		if res.Source == "" {
			res.Source = "recommend"
		}
		res.Words = append(res.Words, w)
	}
	if res.Source == "" || res.Source == "inbox" || res.Source == "hot" {
		for _, w := range res.Words {
			res.HotWords = append(res.HotWords, w.Word)
		}
	}
	if res.Source == "" && len(res.Words) > 0 {
		res.Source = "hot"
	}
	return res, nil
}

// suggestNestedWords extracts the dy group form's inner word items.
func suggestNestedWords(c *contracts.Contract, rec map[string]any) []SuggestWord {
	arr, ok := rec["words"].([]any)
	if !ok {
		return nil
	}
	out := make([]SuggestWord, 0, len(arr))
	for _, el := range arr {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		w := SuggestWord{
			Word:     fieldStr(c, "word", m, suggestWordKeyFamilies["word"]),
			ID:       fieldStr(c, "word_id", m, suggestWordKeyFamilies["word_id"]),
			Category: fieldStr(c, "category", m, []string{"type", "search_type"}),
		}
		if v := resolveValue(c, "", m, suggestWordKeyFamilies["challenge_id"]); v != nil {
			w.ChallengeID = asStr(v)
		}
		if w.Word != "" {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pickNonEmpty returns s when non-empty else fallback.
func pickNonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
