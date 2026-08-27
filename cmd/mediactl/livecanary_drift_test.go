package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// liveEnv: a douyin contract tree pointing at a mock platform whose page 2
// shape is controllable (the f2 #435 half-dead-cookie scenario).
func liveEnv(t *testing.T, page2Empty bool) (adaptDir, dataDir string, cleanup func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "search"):
			fmt.Fprint(w, `{"data":[{"id":"it-1","desc":"d"}],"has_more":false,"cursor":"c1"}`)
		case strings.Contains(r.URL.Path, "comment") && r.URL.Query().Get("cursor") != "":
			if page2Empty {
				fmt.Fprint(w, `{"comments":[],"has_more":false,"cursor":"x"}`)
				return
			}
			fmt.Fprint(w, `{"comments":[{"cid":"c2","text":"t"}],"has_more":false,"cursor":"x"}`)
		default:
			// page 1 exactly full (5 = limit): the walk stops at the limit
			// with has_more=true, so the depth probe is what fetches page 2
			fmt.Fprint(w, `{"comments":[{"cid":"c1","text":"t","user":{"uid":"u1","sec_uid":"s1"}},{"cid":"c2"},{"cid":"c3"},{"cid":"c4"},{"cid":"c5"}],"has_more":true,"cursor":"page2"}`)
		}
	}))
	adapt := t.TempDir()
	cdir := filepath.Join(adapt, "contracts")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	contract := fmt.Sprintf(`{
	  "name": "douyin-search", "platform": "douyin", "category": "search", "version": "1",
	  "transport": {"base_url": %q, "path": "/aweme/search/", "method": "GET", "placeholders": ["keyword"]},
	  "binding": {"items": "$.data"}
	}`, srv.URL)
	if err := os.WriteFile(filepath.Join(cdir, "douyin-search.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	comments := fmt.Sprintf(`{
	  "name": "douyin-comments", "platform": "douyin", "category": "comments", "version": "1",
	  "transport": {"base_url": %q, "path": "/aweme/comment/", "method": "GET", "placeholders": ["item_id"]},
	  "binding": {"comments": "$.comments"},
	  "paging": {"cursor_param": "cursor", "count_param": "count", "count_default": 5, "has_more_path": "$.has_more", "next_cursor_path": "$.cursor"}
	}`, srv.URL)
	if err := os.WriteFile(filepath.Join(cdir, "douyin-comments.json"), []byte(comments), 0o644); err != nil {
		t.Fatal(err)
	}
	// fixture+canary not needed for liveCanary (it takes the registry directly)
	reg := contracts.NewRegistry()
	_ = reg
	data := t.TempDir()
	wd, _ := os.Getwd()
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	_ = root
	return adapt, data, func() { srv.Close() }
}

// TestLiveCanaryHalfDeadCookieDetected: page 1 alive + page 2 (200 + empty)
// is flagged as a pagination-depth drift, recorded in the drift report, and
// the run fails (W7-C1 AC-5). AGENT_APP_SECRET unset → filing skip is the
// documented path (no network).
func TestLiveCanaryHalfDeadCookieDetected(t *testing.T) {
	adapt, _, cleanup := liveEnv(t, true)
	defer cleanup()
	t.Setenv("MEDIAMON_ADAPT_DIR", adapt)
	t.Setenv("MEDIAMON_CANARY_COOKIES_DOUYIN", "ttwid=fake-live-cookie")
	t.Setenv("MEDIAMON_CANARY_COOKIES_KUAISHOU", "")
	t.Setenv("MEDIAMON_CANARY_COOKIES_XHS", "")
	t.Setenv("MEDIAMON_SIGNER_URL", "")
	t.Setenv("AGENT_APP_SECRET", "")

	// run from a temp CWD so adapt/reports lands in a sandbox
	wd, _ := os.Getwd()
	sandbox := t.TempDir()
	if err := os.Chdir(sandbox); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, filepath.Join(adapt, "contracts")); err != nil {
		t.Fatal(err)
	}
	err := liveCanary(reg)
	if err == nil {
		t.Fatal("half-dead cookie scenario must fail the live run")
	}
	// drift report written with the masked account
	entries, _ := os.ReadDir(filepath.Join(sandbox, "adapt", "reports"))
	found := false
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "live-canary-drift-") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(sandbox, "adapt", "reports", e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		var drifts []liveDrift
		if jerr := json.Unmarshal(raw, &drifts); jerr != nil {
			t.Fatal(jerr)
		}
		for _, d := range drifts {
			if d.Kind == "pagination-depth" && d.Platform == "douyin" && d.Account == "canary-account(douyin)" {
				found = true
			}
			if strings.Contains(d.Detail, "fake-live-cookie") || strings.Contains(d.Account, "ttwid") {
				t.Fatalf("drift leaks cookie material: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("pagination-depth drift missing from reports: %+v", entries)
	}
}

// TestLiveCanaryAllSkippedIsNotSuccess: with zero cookie secrets the run
// reports documented skips and succeeds vacuously — the skip is explicit
// output, not a silent green (AC-2).
func TestLiveCanaryAllSkippedIsNotSuccess(t *testing.T) {
	adapt, _, cleanup := liveEnv(t, false)
	defer cleanup()
	t.Setenv("MEDIAMON_ADAPT_DIR", adapt)
	t.Setenv("MEDIAMON_CANARY_COOKIES_DOUYIN", "")
	t.Setenv("MEDIAMON_CANARY_COOKIES_KUAISHOU", "")
	t.Setenv("MEDIAMON_CANARY_COOKIES_XHS", "")
	t.Setenv("MEDIAMON_SIGNER_URL", "")

	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, filepath.Join(adapt, "contracts")); err != nil {
		t.Fatal(err)
	}
	if err := liveCanary(reg); err != nil {
		t.Fatalf("all-skipped live run must not error: %v", err)
	}
}
