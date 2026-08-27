// mocksrv.go — a fixture-driven mock platform for the offline matrix lane:
// it serves the repository's golden adapt/fixtures over HTTP, routed by
// contract transport path and paged by the cursor chain each fixture
// declares (max_cursor / cursor / offset), plus inline-registered extreme
// payloads. The collect engine talks to it through contracts remapped to
// the local listener — the exact pattern the engine tests use, lifted from
// internal/testkit into production shape so `lab` can drive it without a
// test binary.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const fixtureHostPrefix = "https://example.invalid"

// canonicalKey normalizes an incoming cursor value for route lookup:
// empty stays empty; integer-valued strings collapse to decimal form so
// "0" / "00" and a bare empty first call land on page one.
func canonicalKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return s
}

// nextCursorValue extracts the served document's next-cursor value,
// checking shallow cursor-carrying keys at root and under data.
func nextCursorValue(doc map[string]any) (string, bool) {
	for _, probe := range []struct {
		d   map[string]any
		key string
	}{
		{doc, "max_cursor"}, {doc, "cursor"}, {doc, "offset"},
	} {
		if v, ok := probe.d[probe.key]; ok && v != nil {
			switch t := v.(type) {
			case float64:
				return strconv.FormatInt(int64(t), 10), true
			case json.Number:
				return t.String(), true
			case string:
				return t, true
			}
		}
	}
	if sub, ok := doc["data"].(map[string]any); ok {
		return nextCursorValue(sub)
	}
	return "", false
}

type mockRoute struct {
	pages     []map[string]any                   // page i is keyed by nextCursor(page[i-1]); page 0 by "" (alias "0")
	bodies    map[string][]byte                  // explicit key overrides registered by extreme rows
	hooks     map[string]func(*http.Request) any // dynamic bodies (echo probes …)
	media     []string                           // media prefixes this path serves before the JSON router
	mediaBody []byte
	keys      []string // telemetry: incoming canonical keys in arrival order
}

// mockPlatform owns the httptest server + all routes.
type mockPlatform struct {
	mu     sync.Mutex
	server *httptest.Server
	routes map[string]*mockRoute
}

func newMockPlatform() *mockPlatform {
	return &mockPlatform{routes: map[string]*mockRoute{}}
}

// Start boots the listener; routes may be added before or after.
func (m *mockPlatform) Start() {
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.routes {
		for k, raw := range r.bodies {
			r.bodies[k] = m.rebase(raw)
		}
	}
}

func (m *mockPlatform) URL() string {
	if m.server == nil {
		panic("mockplatform: Start not called")
	}
	return m.server.URL
}

func (m *mockPlatform) Close() {
	if m.server != nil {
		m.server.Close()
	}
}

// rebase rewrites example.invalid URLs inside fixture bytes onto the live
// mock host so downstream fetches (video play_addr …) stay on-machine.
func (m *mockPlatform) rebase(raw []byte) []byte {
	host := fixtureHostPrefix
	if m.server != nil {
		host = m.server.URL
	}
	return bytes.ReplaceAll(raw, []byte(fixtureHostPrefix), []byte(host))
}

// AddFixtureChain loads fixture files as one paged chain on transport
// path. Page order = argument order; terminal has_more comes from the
// last fixture itself.
func (m *mockPlatform) AddFixtureChain(path string, files ...string) error {
	pages := make([]map[string]any, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("mockplatform: read fixture %s: %w", filepath.Base(f), err)
		}
		var doc map[string]any
		if err := json.Unmarshal(m.rebase(raw), &doc); err != nil {
			return fmt.Errorf("mockplatform: fixture %s: %w", filepath.Base(f), err)
		}
		pages = append(pages, doc)
	}
	return m.AddDocChain(path, pages...)
}

// AddDocChain registers synthetic documents as one paged chain.
func (m *mockPlatform) AddDocChain(path string, docs ...map[string]any) error {
	if len(docs) == 0 {
		return fmt.Errorf("mockplatform: %s: empty chain", path)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[path] = &mockRoute{pages: docs, bodies: map[string][]byte{},
		hooks: map[string]func(*http.Request) any{}}
	return nil
}

// SetBody pins an inline body for (path,key) — extreme rows use it for
// zero-result pages, deleted placeholders, echo probes, media bytes.
func (m *mockPlatform) SetBody(path, key string, doc any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.routeLocked(path)
	r.bodies[key] = m.rebase(raw)
	return nil
}

// SetHook pins a dynamic body provider for (path,key): fn sees the live
// request and its return value is marshaled as the response (echo probes
// assert the engine sends the exact keyword bytes on the wire).
func (m *mockPlatform) SetHook(path, key string, fn func(*http.Request) any) error {
	if fn == nil {
		return fmt.Errorf("mockplatform: %s/%s: nil hook", path, key)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.routeLocked(path)
	r.hooks[key] = fn
	return nil
}

// SetMedia serves literal bytes for any URL whose path carries prefix —
// the download leg's artifact source bytes.
func (m *mockPlatform) SetMedia(prefix string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.routeLocked(prefix)
	r.media = append(r.media, prefix)
	r.mediaBody = body
}

func (m *mockPlatform) routeLocked(path string) *mockRoute {
	if r, ok := m.routes[path]; ok {
		return r
	}
	r := &mockRoute{bodies: map[string][]byte{}, hooks: map[string]func(*http.Request) any{}}
	m.routes[path] = r
	return r
}

// ReceivedKeys returns the incoming canonical cursor keys recorded for
// path, in arrival order (cursor-monotonicity evidence).
func (m *mockPlatform) ReceivedKeys(path string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.routes[path]
	if !ok {
		return nil
	}
	out := make([]string, len(r.keys))
	copy(out, r.keys)
	return out
}

var mockCursorParams = []string{"max_cursor", "cursor", "offset"}

func (m *mockPlatform) serve(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	key := ""
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range mockCursorParams {
		vs := req.URL.Query()[p]
		if len(vs) > 0 {
			key = canonicalKey(vs[0])
			break
		}
	}
	// Media routes answer before JSON routing.
	for _, r := range m.routes {
		for _, prefix := range r.media {
			if strings.HasPrefix(path, prefix) {
				ct := "application/octet-stream"
				if strings.HasSuffix(path, ".mp4") || strings.HasSuffix(path, ".jpg") {
					if strings.HasSuffix(path, ".mp4") {
						ct = "video/mp4"
					} else {
						ct = "image/jpeg"
					}
				}
				w.Header().Set("Content-Type", ct)
				w.Write(r.mediaBody)
				return
			}
		}
	}
	r, ok := m.routes[path]
	if !ok {
		http.Error(w, fmt.Sprintf("mockplatform: no route %q", path), http.StatusNotFound)
		return
	}
	r.keys = append(r.keys, key)
	if hook, ok := r.hooks[key]; ok {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := json.Marshal(hook(req))
		w.Write(raw)
		return
	}
	if raw, ok := r.bodies[key]; ok {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
		return
	}
	// Chain walk: find which page declares arriving key as ITS next cursor;
	// page0 answers "" (plus the common "0" alias).
	want := key
	if want == "0" {
		want = ""
	}
	for i, pg := range r.pages {
		var declares bool
		if i == 0 {
			declares = want == ""
		} else {
			prevNext, _ := nextCursorValue(r.pages[i-1])
			declares = prevNext == want
		}
		if declares {
			raw, _ := json.Marshal(pg)
			w.Header().Set("Content-Type", "application/json")
			w.Write(raw)
			return
		}
	}
	http.Error(w, fmt.Sprintf("mockplatform: %s: unknown cursor key %q", path, key), http.StatusNotFound)
}

// synthIDKeys are object-id fields mutated when synthesizing extra pages:
// every string value under these keys gets the page suffix appended so
// dedupe/continuity assertions see distinct rows per page.
var synthIDKeys = map[string]bool{
	"aweme_id": true, "id": true, "note_id": true, "cid": true,
	"comment_id": true, "photo_id": true, "item_id": true, "user_id": true,
	"group_id": true,
}

type synthOpts struct {
	// CursorStep is added per synthesized index to numeric cursors
	// (default 1000). String cursors get "-pN".
	CursorStep int64
}

// synthPages clones base into n distinct paged documents: later pages carry
// suffixed ids, decremented create_time values, advanced cursors, and only
// the last page ends has_more=false. The original pagination location
// (root or nested data.*) is preserved wherever the key already exists.
func synthPages(base map[string]any, n int, opts synthOpts) []map[string]any {
	step := opts.CursorStep
	if step == 0 {
		step = 1000
	}
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		doc := transformDoc(base, appendSuffix(i))
		setHasMore(doc, i < n-1)
		advanceCursor(doc, int64(i)*step)
		out = append(out, doc)
	}
	return out
}

// transformDoc deep-copies any JSON-shaped value through fn.
func transformDoc(v any, fn func(any) any) map[string]any {
	return fn(v).(map[string]any)
}

// appendSuffix returns a copier that appends "-p<i>" to every id-keyed
// string once per level and decrements create_time by the page delta.
func appendSuffix(i int) func(any) any {
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			cp := make(map[string]any, len(t))
			for k, val := range t {
				if synthIDKeys[k] {
					if s, ok := val.(string); ok && s != "" {
						val = fmt.Sprintf("%s-p%d", s, i)
					}
				}
				if k == "create_time" || k == "time" || k == "timestamp" {
					if n, ok := toNum(val); ok {
						val = n - float64(i*10)
					}
				}
				cp[k] = walk(val)
			}
			return cp
		case []any:
			cp := make([]any, len(t))
			for j, el := range t {
				cp[j] = walk(el)
			}
			return cp
		default:
			return v
		}
	}
	return walk
}

func toNum(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// setHasMore writes b where the doc already carries a has_more key (root
// or nested data.*), preserving the fixture family's layout.
func setHasMore(doc map[string]any, b bool) {
	if _, ok := doc["has_more"]; ok {
		doc["has_more"] = b
	}
	if sub, ok := doc["data"].(map[string]any); ok {
		setHasMore(sub, b)
	}
}

// advanceCursor shifts existing numeric/string cursor keys forward by d
// (root or nested data.*).
func advanceCursor(doc map[string]any, d int64) {
	for _, k := range []string{"max_cursor", "cursor", "offset"} {
		if v, ok := doc[k]; ok {
			switch t := v.(type) {
			case float64:
				doc[k] = t + float64(d)
			case json.Number:
				n, _ := t.Float64()
				doc[k] = n + float64(d)
			case string:
				if n, err := strconv.ParseInt(t, 10, 64); err == nil {
					doc[k] = strconv.FormatInt(n+d, 10)
				} else if d > 0 {
					doc[k] = fmt.Sprintf("%s-p%d", t, d/int64(1000))
				}
			}
		}
	}
	if sub, ok := doc["data"].(map[string]any); ok {
		advanceCursor(sub, d)
	}
}

// locateArray resolves a binding items path ("$.data.notes") inside doc.
func locateArray(doc map[string]any, bindingPath string) ([]any, bool) {
	if bindingPath == "" || bindingPath == "$" {
		return nil, false
	}
	cur := any(doc)
	for _, seg := range strings.Split(strings.TrimPrefix(bindingPath, "$."), ".") {
		if seg == "" {
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = m[seg]
	}
	arr, ok := cur.([]any)
	return arr, ok
}
