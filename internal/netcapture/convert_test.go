package netcapture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// testRegistry builds a one-contract registry matching the fixture paths.
func testRegistry(t *testing.T) *contracts.Registry {
	t.Helper()
	reg := contracts.NewRegistry()
	contract := `{
	  "name": "douyin-comments", "platform": "douyin", "category": "comments", "version": "1",
	  "transport": {"base_url": "https://www.douyin.com", "path": "/aweme/v1/web/comment/list/", "method": "GET"},
	  "binding": {"comments": "$.comments"},
	  "paging": {"cursor_param": "cursor", "has_more_path": "$.has_more", "next_cursor_path": "$.cursor"}
	}`
	c, err := contracts.Load("douyin-comments.json", []byte(contract))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(c); err != nil {
		t.Fatal(err)
	}
	return reg
}

// harWith builds a one-entry HAR.
func harWith(url, body string) *HAR {
	return &HAR{Log: HARLog{Version: "1.2", Entries: []Entry{{
		Request:  Req{Method: "GET", URL: url},
		Response: Resp{Status: 200, Body: body},
	}}}}
}

// TestConvertRedactsCredentials: cookie/authorization headers and signing
// query params never survive into a candidate (AC-4, INV-6).
func TestConvertRedactsCredentials(t *testing.T) {
	h := harWith("https://www.douyin.com/aweme/v1/web/comment/list/?item_id=i1&cursor=5&a_bogus=XYZ&msToken=AAA",
		`{"comments":[{"cid":"c1"}],"has_more":false,"cursor":"5","sessionid":"SESSID-abcdef0123456789","ttwid":"TTWID-0123456789abcdef"}`)
	cands, errs := ConvertHAR(h, testRegistry(t))
	if len(errs) != 0 || len(cands) != 1 {
		t.Fatalf("cands=%d errs=%v", len(cands), errs)
	}
	c := cands[0]
	if c.Contract != "douyin-comments" {
		t.Fatalf("contract = %q", c.Contract)
	}
	if _, ok := c.Query["a_bogus"]; ok {
		t.Fatal("a_bogus survived redaction")
	}
	if _, ok := c.Query["msToken"]; ok {
		t.Fatal("msToken survived redaction")
	}
	if c.Query["item_id"] != "i1" || c.Query["cursor"] != "5" {
		t.Fatalf("non-signature query dropped: %v", c.Query)
	}
	raw, _ := json.Marshal(c.Body)
	for _, leaked := range []string{"SESSID", "TTWID", "sessionid", "ttwid", "a_bogus", "XYZ"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("credential %q leaked into fixture: %s", leaked, raw)
		}
	}
	joined := strings.Join(c.Stripped, ",")
	for _, want := range []string{"query:a_bogus", "query:msToken"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("strip ledger missing %q: %v", want, c.Stripped)
		}
	}
}

// TestConvertFieldCompletenessGate: a 2xx body whose contract binding is
// absent yields an explicit error — no half-converted fixture (AC-1).
func TestConvertFieldCompletenessGate(t *testing.T) {
	h := harWith("https://www.douyin.com/aweme/v1/web/comment/list/?item_id=i1", `{"unrelated": true}`)
	cands, errs := ConvertHAR(h, testRegistry(t))
	if len(errs) != 1 || len(cands) != 0 {
		t.Fatalf("cands=%d errs=%v", len(cands), errs)
	}
	if !strings.Contains(errs[0].Error(), "field completeness gate") {
		t.Fatalf("err = %v", errs[0])
	}
}

// TestConvertNonJSONObject: non-JSON bodies error explicitly.
func TestConvertNonJSONObject(t *testing.T) {
	h := harWith("https://www.douyin.com/aweme/v1/web/comment/list/", `<html>login wall</html>`)
	_, errs := ConvertHAR(h, testRegistry(t))
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "not a JSON object") {
		t.Fatalf("errs = %v", errs)
	}
}

// TestProposeEndToEnd: matched candidate → proposal with JSON patch + issue
// draft; the candidate feeds contracts.Diff (AC-2/AC-3).
func TestProposeEndToEnd(t *testing.T) {
	reg := testRegistry(t)
	h := harWith("https://www.douyin.com/aweme/v1/web/comment/list/?item_id=i1&cursor=5",
		`{"comments":[{"cid":"c1","text":"hi"}],"has_more":false,"cursor":"5"}`)
	cands, errs := ConvertHAR(h, reg)
	if len(errs) != 0 || len(cands) != 1 {
		t.Fatalf("errs=%v", errs)
	}
	p, err := Propose(cands[0], reg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Contract != "douyin-comments" || len(p.Patch) != 2 {
		t.Fatalf("proposal = %+v", p)
	}
	if p.Patch[0].Path != "/adapt/fixtures/douyin-comments.1.json" || p.Patch[0].Op != "add" {
		t.Fatalf("patch[0] = %+v", p.Patch[0])
	}
	for _, want := range []string{"契约补丁提案", "douyin-comments", "提案≠变更", "adapt diff"} {
		if !strings.Contains(p.IssueDraft, want) {
			t.Fatalf("draft missing %q:\n%s", want, p.IssueDraft)
		}
	}
}

// TestProposeUnmatchedFailsClosed: unmatched path refuses to propose.
func TestProposeUnmatchedFailsClosed(t *testing.T) {
	c := Candidate{Method: "GET", Path: "/nope"}
	if _, err := Propose(c, testRegistry(t), 1); err == nil {
		t.Fatal("unmatched candidate must fail closed")
	}
}

// TestVisionChainMock (AC-5): a synthetic vision-run outcome — a fake
// screenshot walk whose network trace is a HAR — flows through the
// converter into a proposal (the W6-C1 ↔ W6-C2 hand-off shape).
func TestVisionChainMock(t *testing.T) {
	reg := testRegistry(t)
	// the "vision-driven device run" observed one API exchange
	h := &HAR{Log: HARLog{Version: "1.2", Entries: []Entry{{
		StartedDateTime: "2026-08-26T00:00:00Z",
		Request: Req{Method: "GET", URL: "https://www.douyin.com/aweme/v1/web/comment/list/?item_id=i9&cursor=0&a_bogus=Z9",
			Headers: []KV{{Name: "Cookie", Value: "ttwid=SECRET-TTWID; sessionid=SECRET-SESS"}}},
		Response: Resp{Status: 200, Body: `{"comments":[{"cid":"v1"}],"has_more":true,"cursor":"9"}`},
	}}}}
	cands, errs := ConvertHAR(h, reg)
	if len(errs) != 0 || len(cands) != 1 {
		t.Fatalf("errs=%v cands=%d", errs, len(cands))
	}
	p, err := Propose(cands[0], reg, 2)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "SECRET") {
		t.Fatalf("vision chain leaked credentials into proposal: %s", raw)
	}
}

// TestConvertWritesGitleaksCleanFile: the on-disk candidate passes a
// credential-pattern scan (INV-6 gitleaks-cleanliness proxy).
func TestConvertWritesGitleaksCleanFile(t *testing.T) {
	h := harWith("https://www.douyin.com/aweme/v1/web/comment/list/?item_id=i1",
		`{"comments":[{"cid":"c1"}],"has_more":false,"cursor":"","access_token":"ghp_0123456789abcdefghijklmnopqrstuvwxyz"}`)
	cands, _ := ConvertHAR(h, testRegistry(t))
	if len(cands) != 1 {
		t.Fatalf("cands = %d", len(cands))
	}
	dir := t.TempDir()
	raw, _ := json.MarshalIndent(cands[0], "", "  ")
	path := filepath.Join(dir, "candidate.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	onDisk, _ := os.ReadFile(path)
	// key NAMES in the strip ledger are fine; the secret VALUE must be gone
	for _, pat := range []string{"ghp_", "0123456789abcdef"} {
		if strings.Contains(string(onDisk), pat) {
			t.Fatalf("gitleaks-pattern %q present in fixture", pat)
		}
	}
}
