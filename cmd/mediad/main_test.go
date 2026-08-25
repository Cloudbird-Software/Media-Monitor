package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/core"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

// writeDemoAdapt lays out a temp adapt tree whose demo-search contract
// points at srvURL (an httptest server): the collect engine is fully mocked
// through that base URL plus a canary contract/fixture for the dashboard.
func writeDemoAdapt(t *testing.T, srvURL string) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("contracts/demo-search.json", `{
	  "name": "demo-search",
	  "platform": "demo",
	  "category": "search",
	  "version": "1",
	  "doc": "test-only contract",
	  "transport": {"base_url": "`+srvURL+`", "path": "/api/search/{keyword}/", "method": "GET", "placeholders": ["keyword"]},
	  "binding": {"items": "$.data"},
	  "paging": {"cursor_param": "offset", "count_param": "count", "count_default": 20, "has_more_path": "$.has_more", "next_cursor_path": "$.cursor"}
	}`)
	mustWrite("fixtures/fixture.json", `{"data": [{"id": "1", "desc": "first"}]}`)
	mustWrite("canaries/one.json", `{"canaries": [{"name": "demo-canary", "contract": "demo-search", "kind": "items", "fixture": "fixture.json", "expect": ["data"]}]}`)
	return dir
}

// newTestDaemon wires a daemon against the given adapt dir and returns it
// with its routes mounted on an httptest server.
func newTestDaemon(t *testing.T, dataDir, adaptDir string) (*daemon, *httptest.Server) {
	t.Helper()
	t.Setenv("MEDIAMON_ADAPT_DIR", adaptDir)
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	counters := obs.NewCounterMap()
	d := &daemon{runner: core.NewRunner(st, counters), counters: counters}
	d.wireAdapt()
	ts := httptest.NewServer(d.routes())
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})
	return d, ts
}

// routes exposes the handler tree (equivalent to the mux assembled in main).
func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obs.HealthNow(true))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, d.counters.MetricsText())
	})
	mux.HandleFunc("/api/v1/tasks", taskHandler(d.runner))
	mux.HandleFunc("/api/v1/collect/", d.collectHandler)
	mux.HandleFunc("/", d.dashboardHandler)
	return mux
}

func postJSON(t *testing.T, ts *httptest.Server, path string, body map[string]any) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func TestCollectSearchEndpoint(t *testing.T) {
	apiCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		if r.URL.Path != "/api/search/golang/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("count") != "3" {
			t.Errorf("count param = %q", r.URL.Query().Get("count"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"1","desc":"first","create_time":1700000000,"author":{"user_id":"u1","sec_uid":"s1","nickname":"nick"}}],"has_more":false,"cursor":"c1"}`)
	}))
	defer api.Close()

	_, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), writeDemoAdapt(t, api.URL))
	resp, b := postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo", "keyword": "golang", "limit": 3})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	var out struct {
		Items  []map[string]any `json:"items"`
		Cursor map[string]any   `json:"cursor"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0]["id"] != "1" || out.Items[0]["desc"] != "first" {
		t.Fatalf("items = %v", out.Items)
	}
	if out.Cursor["has_more"] != false {
		t.Fatalf("cursor = %v", out.Cursor)
	}
	if apiCalls != 1 {
		t.Fatalf("api calls = %d", apiCalls)
	}
}

func TestCollectEndpointsValidationAndErrors(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[],"has_more":false}`)
	}))
	defer api.Close()
	_, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), writeDemoAdapt(t, api.URL))

	// GET is not allowed.
	resp, err := http.Get(ts.URL + "/api/v1/collect/search")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	// Unknown op.
	resp, b := postJSON(t, ts, "/api/v1/collect/nope", map[string]any{})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown op status = %d, body = %s", resp.StatusCode, b)
	}
	// Unknown platform.
	resp, b = postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "nope", "keyword": "k"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "platform") {
		t.Fatalf("platform status = %d, body = %s", resp.StatusCode, b)
	}
	// Broken body.
	resp, err = http.Post(ts.URL+"/api/v1/collect/search", "application/json", strings.NewReader("{"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("broken body status = %d", resp.StatusCode)
	}
	// Missing keyword: parameter validation happens in the engine path, so
	// the handler answers 500 carrying the message.
	resp, b = postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo"})
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(b), "keyword is required") {
		t.Fatalf("missing keyword status = %d, body = %s", resp.StatusCode, b)
	}
}

func TestCollectEndpointEmptyPageSucceeds(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[],"has_more":false}`)
	}))
	defer api.Close()
	_, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), writeDemoAdapt(t, api.URL))
	// Empty lists are valid zero-record pages (review finding #6): the
	// endpoint returns 200 with an empty items array, not a contract error.
	resp, b := postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo", "keyword": "k"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `"items":[]`) {
		t.Fatalf("body = %s", b)
	}
}

func TestCollectEngineUnavailable(t *testing.T) {
	_, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "missing-adapt"))
	resp, b := postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo", "keyword": "k"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "collect engine unavailable") {
		t.Fatalf("body = %s", b)
	}
}

func TestDashboardTaskStatsAndCanary(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"1"}],"has_more":false}`)
	}))
	defer api.Close()
	_, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), writeDemoAdapt(t, api.URL))

	// One collect call so a counter shows up in the metrics block.
	if resp, b := postJSON(t, ts, "/api/v1/collect/search", map[string]any{"platform": "demo", "keyword": "k"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("collect status = %d, body = %s", resp.StatusCode, b)
	}

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, want := range []string{"media-monitor daemon", "tasks", "offline canary summary", "demo-search: healthy", "collect.fetch 1", "metrics"} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard misses %q", want)
		}
	}
}

func TestHealthzAndMetrics(t *testing.T) {
	d, ts := newTestDaemon(t, filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "missing-adapt"))
	d.counters.Inc("test.counter", 3)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"status":"ok"`) {
		t.Fatalf("healthz = %s", b)
	}
	resp, err = http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "test.counter 3") {
		t.Fatalf("metrics = %q", b)
	}
}
