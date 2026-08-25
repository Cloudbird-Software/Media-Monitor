// Package contracts — drift detection: compare a live/observed JSON response
// against the contract binding and emit machine-actionable issues.
package contracts

import (
	"fmt"
	"strings"
)

// DriftIssue is one concrete mismatch between observed data and contract.
type DriftIssue struct {
	Severity string `json:"severity"` // error|warning
	Code     string `json:"code"`     // missing_path | type_mismatch | paging_missing | required_field_missing
	Detail   string `json:"detail"`
	Path     string `json:"path,omitempty"`
}

func (d DriftIssue) String() string {
	return fmt.Sprintf("[%s] %s: %s", d.Severity, d.Code, d.Detail)
}

// DiffReport summarizes drift for one contract vs one observed document.
type DiffReport struct {
	Contract string       `json:"contract"`
	Observed string       `json:"observed"` // fixture/observation id
	Issues   []DriftIssue `json:"issues"`
}

func (r *DiffReport) Healthy() bool {
	for _, i := range r.Issues {
		if i.Severity == "error" {
			return false
		}
	}
	return true
}

// Diff verifies the contract against observed doc. kind selects which binder
// output to check: items|comments|users|members.
func Diff(c *Contract, observed any, kind string) *DiffReport {
	r := &DiffReport{Contract: c.Name}
	rawDoc, ok := observed.(map[string]any)
	if !ok {
		r.Issues = append(r.Issues, DriftIssue{Severity: "error", Code: "type_mismatch", Detail: "observed document is not a JSON object"})
		return r
	}

	checkBinding := func(name, pathRaw string) {
		if pathRaw == "" {
			r.Issues = append(r.Issues, DriftIssue{Severity: "warning", Code: "missing_path",
				Detail: fmt.Sprintf("contract declares no binding for %s", name)})
			return
		}
		p, err := ParsePath(pathRaw)
		if err != nil {
			r.Issues = append(r.Issues, DriftIssue{Severity: "error", Code: "missing_path", Detail: err.Error(), Path: pathRaw})
			return
		}
		vs := p.Select(rawDoc)
		if len(vs) == 0 {
			r.Issues = append(r.Issues, DriftIssue{Severity: "error", Code: "missing_path",
				Detail: fmt.Sprintf("%s path %s not present in observed payload", name, pathRaw), Path: pathRaw})
			return
		}
		if kind == name || kind == "" {
			if list, ok := vs[0].([]any); ok && len(list) == 0 {
				r.Issues = append(r.Issues, DriftIssue{Severity: "warning", Code: "missing_path",
					Detail: fmt.Sprintf("%s list is empty in observed payload", name), Path: pathRaw})
			}
		}
	}

	checkBinding("items", c.Binding.Items)
	checkBinding("comments", c.Binding.Comments)
	checkBinding("users", c.Binding.Users)
	checkBinding("members", c.Binding.Members)

	for field, pathRaw := range c.Binding.Fields {
		p, err := ParsePath(pathRaw)
		if err != nil {
			r.Issues = append(r.Issues, DriftIssue{Severity: "error", Code: "missing_path", Detail: err.Error(), Path: pathRaw})
			continue
		}
		if len(p.Select(rawDoc)) == 0 {
			r.Issues = append(r.Issues, DriftIssue{Severity: "warning", Code: "required_field_missing",
				Detail: fmt.Sprintf("field %q missing", field), Path: pathRaw})
		}
	}

	// Pagination sanity: cursor param must either be absent or present as a value.
	if c.Paging.HasMorePath != "" {
		p, err := ParsePath(c.Paging.HasMorePath)
		if err != nil {
			r.Issues = append(r.Issues, DriftIssue{Severity: "error", Code: "paging_missing", Detail: err.Error()})
		} else {
			vs := p.Select(rawDoc)
			if len(vs) == 0 {
				r.Issues = append(r.Issues, DriftIssue{Severity: "warning", Code: "paging_missing",
					Detail: fmt.Sprintf("paging.has_more_path %s absent", c.Paging.HasMorePath)})
			} else if _, isBool := vs[0].(bool); !isBool {
				if num, isNum := vs[0].(float64); !isNum || (num != 0 && num != 1) {
					r.Issues = append(r.Issues, DriftIssue{Severity: "warning", Code: "paging_missing",
						Detail: fmt.Sprintf("paging.has_more_path %s is not boolean", c.Paging.HasMorePath)})
				}
			}
		}
	}
	return r
}

// Summarize returns a one-line render of a report set.
func Summarize(reports []*DiffReport) string {
	var b strings.Builder
	for _, r := range reports {
		state := "healthy"
		if !r.Healthy() {
			state = "UNHEALTHY"
		}
		b.WriteString(fmt.Sprintf("%s: %s (%d issues)\n", r.Contract, state, len(r.Issues)))
		for _, i := range r.Issues {
			b.WriteString("  - " + i.String() + "\n")
		}
	}
	return b.String()
}
