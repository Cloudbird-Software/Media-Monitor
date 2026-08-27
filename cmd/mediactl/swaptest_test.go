package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestF2AdapterScoresAgainstFixture: the adapter computes a conformance
// ratio from f2's parameter table (fixture corpus served over httptest),
// with a details line naming the pin (AC-1).
func TestF2AdapterScoresAgainstFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`PARAMS = {
  "sec_user_id": "",
  "max_cursor": 0,
  "count": 20,
  "device_platform": "webapp",
  "aid": "6383",
  "channel": "channel_pc_web",
  "keyword": "",
  "offset": 0,
  "search_channel": "aweme_general",
  "item_id": "",
  "cursor": 0,
  "aweme_id": "",
}
SOMETHING_ELSE = "unrelated"
`))
	}))
	defer srv.Close()
	a := f2ParamAdapter{hc: srv.Client(), token: ""}
	// point the adapter at the fixture server by overriding the fetch target
	oldFetch := fetchUpstreamFileDefault
	fetchUpstreamFileDefault = func(ctx context.Context, hc *http.Client, slug, ref, path, token string) ([]byte, error) {
		resp, err := hc.Get(srv.URL + "/corpus")
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	defer func() { fetchUpstreamFileDefault = oldFetch }()
	rate, detail, err := a.Score(context.Background(), "pin123", scoredContracts())
	if err != nil {
		t.Fatal(err)
	}
	if rate != 1.0 {
		t.Fatalf("rate = %v, want 1.0 (all keys present)", rate)
	}
	if !strings.Contains(detail, "pin123") {
		t.Fatalf("detail = %q", detail)
	}
}

// TestSwapTestExplicitErrors: unknown slug / no adapter are explicit errors
// (AC-3, fail-closed no hang).
func TestSwapTestExplicitErrors(t *testing.T) {
	// the registry path is CWD-relative: run from the repo root
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()
	if err := upstreamSwapTest([]string{"not/a-slug"}); err == nil || !strings.Contains(err.Error(), "not in registry") {
		t.Fatalf("err = %v", err)
	}
	if err := upstreamSwapTest([]string{"showlab/ShowUI"}); err == nil || !strings.Contains(err.Error(), "no adapter") {
		t.Fatalf("err = %v", err)
	}
}

// TestSwapScoreJSONShape: the score carries exactly the three headline
// fields (AC-1/AC-2 machine shape).
func TestSwapScoreJSONShape(t *testing.T) {
	s := SwapScore{Slug: "a/b", SuccessRate: 0.9, FreshnessDays: 3, LicenseVerdict: "allowed"}
	raw := []byte(mustJSON(s))
	for _, want := range []string{`"success_rate":0.9`, `"freshness_days":3`, `"license_verdict":"allowed"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("score JSON missing %s: %s", want, raw)
		}
	}
}
