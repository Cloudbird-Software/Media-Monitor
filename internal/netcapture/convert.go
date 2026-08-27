// HAR → candidate-fixture converter (IR-MM-0001 AC-15 / BEH-11..13, track
// B second half): turns captured traffic into reviewable contract-patch
// proposals. Redaction is a precondition (INV-6): credential headers and
// signing params never survive into a candidate fixture. Proposals are
// proposals — this package never mutates contracts.
package netcapture

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// redactHeaders are request/response header names stripped before a
// candidate fixture is formed (case-insensitive).
var redactHeaders = map[string]bool{
	"cookie": true, "set-cookie": true, "authorization": true,
	"x-s": true, "x-t": true, "x-s-common": true, "x-kh": true,
	"x-ladon": true, "x-argus": true, "x-gorgon": true,
}

// redactQuery are query params stripped from candidates (per-request
// signatures and device fingerprints — regenerated at collect time).
var redactQuery = map[string]bool{
	"a_bogus": true, "msToken": true, "X-Bogus": true, "_signature": true,
	"verifyFp": true, "fp": true, "webid": true,
}

// Candidate is one converted HAR entry: the matched contract (if any), the
// redacted request shape, and the golden response body.
type Candidate struct {
	Contract string            `json:"contract,omitempty"` // matched contract name
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Query    map[string]string `json:"query,omitempty"` // redacted
	Body     map[string]any    `json:"body"`            // parsed golden response
	Stripped []string          `json:"stripped"`        // names removed during redaction
}

// PatchOp is one JSON-patch operation of a proposal.
type PatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// Proposal is the reviewable output: a JSON patch plus an issue draft. It
// is never applied automatically — a human/agent reviews and PRs it.
type Proposal struct {
	Contract   string    `json:"contract"`
	Patch      []PatchOp `json:"patch"`
	IssueDraft string    `json:"issue_draft"`
}

// ConvertHAR converts every JSON-bodied entry of a capture into candidates,
// matching each against the registry's contract paths. Non-JSON bodies or
// entries whose contract primary binding does not resolve in the body are
// reported as explicit errors per entry (never silently dropped, never
// half-converted).
func ConvertHAR(h *HAR, reg *contracts.Registry) ([]Candidate, []error) {
	var out []Candidate
	var errs []error
	for i, e := range h.Log.Entries {
		c, err := convertEntry(e, reg)
		if err != nil {
			errs = append(errs, fmt.Errorf("entry %d (%s %s): %w", i, e.Request.Method, e.Request.URL, err))
			continue
		}
		out = append(out, *c)
	}
	return out, errs
}

func convertEntry(e Entry, reg *contracts.Registry) (*Candidate, error) {
	u, err := url.Parse(e.Request.URL)
	if err != nil {
		return nil, fmt.Errorf("bad url: %w", err)
	}
	c := &Candidate{Method: e.Request.Method, Path: u.Path, Query: map[string]string{}}
	// redact query
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if redactQuery[k] {
			c.Stripped = append(c.Stripped, "query:"+k)
			continue
		}
		c.Query[k] = q.Get(k)
	}
	// redact headers (record what was stripped)
	for _, kv := range append(e.Request.Headers, e.Response.Headers...) {
		if redactHeaders[strings.ToLower(kv.Name)] {
			c.Stripped = append(c.Stripped, "header:"+kv.Name)
		}
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(e.Response.Body), &body); err != nil {
		return nil, fmt.Errorf("response body is not a JSON object: %w", err)
	}
	// deep redaction of credential-looking values inside the body
	c.Stripped = append(c.Stripped, redactBody(body)...)
	c.Body = body
	// contract match by path + method
	for _, name := range reg.List() {
		ct, ok := reg.Get(name)
		if !ok || ct.Transport.Method != e.Request.Method {
			continue
		}
		if ct.Transport.Path != u.Path {
			continue
		}
		c.Contract = name
		// completeness: the primary binding must resolve in the body
		if kind, raw := bindingOf(ct); raw != "" {
			p, perr := contracts.ParsePath(raw)
			if perr != nil {
				return nil, fmt.Errorf("contract %s bad binding: %w", name, perr)
			}
			if len(p.Select(body)) == 0 {
				return nil, fmt.Errorf("contract %s %s binding %s missing from body (field completeness gate)", name, kind, raw)
			}
		}
		break
	}
	return c, nil
}

// bindingOf returns the contract's primary list binding.
func bindingOf(ct *contracts.Contract) (string, string) {
	switch {
	case ct.Binding.Items != "":
		return "items", ct.Binding.Items
	case ct.Binding.Comments != "":
		return "comments", ct.Binding.Comments
	case ct.Binding.Users != "":
		return "users", ct.Binding.Users
	case ct.Binding.Members != "":
		return "members", ct.Binding.Members
	}
	return "", ""
}

// bodySecretKeys are body keys whose values are credentials.
var bodySecretKeys = map[string]bool{
	"token": true, "access_token": true, "refresh_token": true,
	"sessionid": true, "sessionid_ss": true, "sid_guard": true,
	"ttwid": true, "web_session": true, "authorization": true,
}

// redactBody removes credential-looking keys in place and returns the list
// of removed key paths.
func redactBody(body map[string]any) []string {
	var removed []string
	var walk func(m map[string]any, prefix string)
	walk = func(m map[string]any, prefix string) {
		for k, v := range m {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			if bodySecretKeys[strings.ToLower(k)] {
				delete(m, k)
				removed = append(removed, "body:"+path)
				continue
			}
			if s, ok := v.(string); ok && looksLikeCredential(k, s) {
				delete(m, k)
				removed = append(removed, "body:"+path)
				continue
			}
			if child, ok := v.(map[string]any); ok {
				walk(child, path)
			}
		}
	}
	walk(body, "")
	sort.Strings(removed)
	return removed
}

// looksLikeCredential flags long high-entropy cookie/token-shaped values
// under credential-ish key names.
func looksLikeCredential(key, v string) bool {
	lk := strings.ToLower(key)
	if !(strings.Contains(lk, "cookie") || strings.Contains(lk, "token") || strings.Contains(lk, "sig")) {
		return false
	}
	return len(v) >= 32 && !strings.HasPrefix(v, "http")
}

// Propose turns a matched candidate into a reviewable proposal: a JSON
// patch adding the candidate as fixture N+1 plus an issue draft carrying
// the adapt diff report (healthy or drifted).
func Propose(c Candidate, reg *contracts.Registry, fixtureN int) (*Proposal, error) {
	if c.Contract == "" {
		return nil, fmt.Errorf("candidate (path %s) matched no contract — nothing to propose against", c.Path)
	}
	ct, ok := reg.Get(c.Contract)
	if !ok {
		return nil, fmt.Errorf("contract %q vanished", c.Contract)
	}
	fixtureName := fmt.Sprintf("%s.%d.json", c.Contract, fixtureN)
	patch := []PatchOp{
		{Op: "add", Path: "/adapt/fixtures/" + fixtureName, Value: c.Body},
		{Op: "add", Path: "/adapt/canaries/offline.json/canaries/-",
			Value: map[string]any{"name": c.Contract + "-captured-" + fmt.Sprint(fixtureN), "contract": c.Contract,
				"kind": "items", "fixture": fixtureName}},
	}
	kind, _ := bindingOf(ct)
	rep := contracts.Diff(ct, c.Body, kind)
	draft := fmt.Sprintf(`### 契约补丁提案（netcapture 捕获 → 候选 fixture）

- 契约：%s（%s %s）
- 候选 fixture：adapt/fixtures/%s（响应体已脱敏：剥离 %d 处凭据/签名元素）
- adapt diff：%d 个问题
%s
> 提案≠变更：审后走 C1 PR 采纳（补丁 JSON 见本提案 patch 字段）。`,
		c.Contract, c.Method, c.Path, fixtureName, len(c.Stripped), len(rep.Issues), renderIssues(rep.Issues))
	return &Proposal{Contract: c.Contract, Patch: patch, IssueDraft: draft}, nil
}

func renderIssues(issues []contracts.DriftIssue) string {
	var b strings.Builder
	for _, i := range issues {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", i.Severity, i.Code, i.Detail)
	}
	if len(issues) == 0 {
		return "- （与既有契约一致——零漂移，fixture 可直接作为金样补充）\n"
	}
	return b.String()
}
