package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getBody(t *testing.T, url string) ([]byte, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode
}

// TestCanonicalKeyNormalizesIntegers: integer-valued cursors collapse so
// "0" from a first explicit call lands where the engine's initial empty
// cursor lands; non-numeric cursors stay verbatim.
func TestCanonicalKeyNormalizesIntegers(t *testing.T) {
	if canonicalKey("00") != "0" || canonicalKey(" 12 ") != "12" {
		t.Fatalf("int normalization broken: %q %q", canonicalKey("00"), canonicalKey(" 12 "))
	}
	if canonicalKey("") != "" {
		t.Fatalf("empty must stay empty, got %q", canonicalKey(""))
	}
	if canonicalKey("exp-cursor-1") != "exp-cursor-1" {
		t.Fatal("string cursors must stay verbatim")
	}
}

// TestNextCursorValueRootAndNested: max_cursor/cursor/offset are found at
// root or one level down under data.
func TestNextCursorValueRootAndNested(t *testing.T) {
	if v, ok := nextCursorValue(map[string]any{"max_cursor": float64(17)}); !ok || v != "17" {
		t.Fatalf("root numeric: %q %v", v, ok)
	}
	nested := map[string]any{"data": map[string]any{"cursor": "c-9"}}
	if v, ok := nextCursorValue(nested); !ok || v != "c-9" {
		t.Fatalf("nested string: %q %v", v, ok)
	}
	if _, ok := nextCursorValue(map[string]any{}); ok {
		t.Fatal("absent cursor must be ok=false")
	}
}

// TestMockPlatformChainServing: pages serve by their predecessor's declared
// next cursor; telemetry records arrival order.
func TestMockPlatformChainServing(t *testing.T) {
	mp := newMockPlatform()
	defer mp.Close()
	pages := []map[string]any{
		{"comments": []any{map[string]any{"cid": "a"}}, "has_more": true, "cursor": "n1"},
		{"comments": []any{map[string]any{"cid": "b"}}, "has_more": false, "cursor": ""},
	}
	mp.Start()
	if err := mp.AddDocChain("/p/", pages...); err != nil {
		t.Fatal(err)
	}
	first, code := getBody(t, mp.URL()+"/p/?cursor=")
	if code != 200 || !bytes.Contains(first, []byte(`"cid":"a"`)) {
		t.Fatalf("page0 wrong: code=%d body=%s", code, first)
	}
	second, code := getBody(t, mp.URL()+"/p/?cursor=n1")
	if code != 200 || !bytes.Contains(second, []byte(`"cid":"b"`)) {
		t.Fatalf("page1 wrong: code=%d body=%s", code, second)
	}
	keys := mp.ReceivedKeys("/p/")
	if len(keys) != 2 || keys[0] != "" || keys[1] != "n1" {
		t.Fatalf("telemetry keys = %v", keys)
	}
	if _, code = getBody(t, mp.URL()+"/p/?cursor=bogus"); code != 404 {
		t.Fatalf("unknown cursor key should 404, got %d", code)
	}
}

// TestMockPlatformFixtureRebase: example.invalid URLs inside fixtures are
// rewritten onto the live listener.
func TestMockPlatformFixtureRebase(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close()
	if base == fixtureHostPrefix {
		t.Skip("degenerate test server url")
	}
	mp := newMockPlatform()
	defer mp.Close()
	doc := map[string]any{"avatar_url": fixtureHostPrefix + "/x.jpg"}
	mp.Start()
	if err := mp.SetBody("/r", "", doc); err != nil {
		t.Fatal(err)
	}
	raw, _ := getBody(t, mp.URL()+"/r?x=1")
	if bytes.Contains(raw, []byte(fixtureHostPrefix)) {
		t.Fatalf("fixture host not rebased: %s", raw)
	}
	if !bytes.Contains(raw, []byte(mp.URL())) {
		t.Fatalf("mock host missing from rebased body: %s", raw)
	}
}

// TestSynthPagesDistinctIDsAndTerminal: synthesized pages carry suffixed
// ids everywhere, advance cursors, and only the last page ends has_more.
func TestSynthPagesDistinctIDsAndTerminal(t *testing.T) {
	base := map[string]any{
		"data": []any{
			map[string]any{"type": 1, "aweme_info": map[string]any{
				"aweme_id": "id-raw", "desc": "d", "create_time": float64(1780000001),
				"author": map[string]any{"sec_uid": "MS4-constant", "nickname": "示例作者"},
			}},
		},
		"cursor": float64(5000), "has_more": true,
	}
	docs := synthPages(base, 3, synthOpts{})
	if len(docs) != 3 {
		t.Fatalf("want 3 pages, got %d", len(docs))
	}
	ids := map[string]bool{}
	for i, d := range docs {
		arr, _ := locateArray(d, "$.data")
		if len(arr) != 1 {
			t.Fatalf("page %d lost items", i)
		}
		item := arr[0].(map[string]any)["aweme_info"].(map[string]any)
		ids[item["aweme_id"].(string)] = true
		wantHasMore := i < 2
		if got, _ := d["has_more"].(bool); got != wantHasMore {
			t.Fatalf("page %d has_more=%v want %v", i, got, wantHasMore)
		}
		ct := item["create_time"].(float64)
		if ct != 1780000001-float64(i*10) {
			t.Fatalf("page %d create_time drift wrong: %v", i, ct)
		}
		author := item["author"].(map[string]any)
		if author["sec_uid"] != "MS4-constant" {
			t.Fatalf("sec_uid must not mutate across pages")
		}
		cur := d["cursor"].(float64)
		if cur != float64(5000+int64(i)*1000) {
			t.Fatalf("page %d cursor=%v want advanced", i, cur)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("ids not distinct per page: %v", ids)
	}
}

// TestMockHookEchoesQuery: dynamic hooks observe the request query — the
// keyword-roundtrip probe depends on it.
func TestMockHookEchoesQuery(t *testing.T) {
	mp := newMockPlatform()
	defer mp.Close()
	mp.Start()
	err := mp.SetHook("/h", "", func(r *http.Request) any {
		return map[string]any{"echo": r.URL.Query().Get("kw"), "seen": true}
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, code := getBody(t, mp.URL()+"/h?kw=W%C3%B6rt")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["echo"] != "Wört" {
		t.Fatalf("echo lost multibyte kw: %s", raw)
	}
}

// TestSetMediaServesLiteralBytes: download-leg artifacts come from here.
func TestSetMediaServesLiteralBytes(t *testing.T) {
	mp := newMockPlatform()
	defer mp.Close()
	mp.SetMedia("/media/x.mp4", []byte("FAKEMP4BYTES"))
	mp.Start()
	raw, code := getBody(t, mp.URL()+"/media/x.mp4")
	if code != 200 || !bytes.Equal(raw, []byte("FAKEMP4BYTES")) {
		t.Fatalf("media serving broken: code=%d body=%q", code, raw)
	}
}
