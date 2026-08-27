// audit_comments.go — comment-author field-completeness audit
// (`mediactl lab audit-comments --store <dir>`): scans a JSONL store's
// comments collection and reports per-field completeness over the twelve
// mandated user fields (IR-MM-0001 AC-19 target >= 90%; model.go's
// field-completeness contract).
//
// Counting rule (documented, conservative where unavoidable):
//   - string fields count complete only when present AND non-empty;
//   - gender counts complete only as 1|2 (0 means "unknown/unbound");
//   - numeric counters count complete when present as JSON numbers —
//     post-marshal a stored record cannot distinguish an explicit zero
//     from an unbound zero, so presence-not-nonzero is the honest official
//     ratio here (collection happens through binders that leave zeros on
//     missing platform paths).
//
// A comment row without a user object still counts toward every field's
// denominator: it represents one author whose fields are all incomplete.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// auditField is one of the twelve mandated user fields with its kind.
type auditField struct {
	Name string
	Kind string // str | gender | num
}

var auditFields = []auditField{
	{"uid", "str"}, {"sec_uid", "str"}, {"short_id", "str"}, {"nickname", "str"},
	{"avatar_url", "str"}, {"signature", "str"}, {"ip_label", "str"},
	{"gender", "gender"}, {"follower_count", "num"}, {"following_count", "num"},
	{"aweme_count", "num"}, {"total_favorited", "num"},
}

// fieldComplete applies the documented counting rule for one field value.
func fieldComplete(f auditField, v any) bool {
	switch f.Kind {
	case "str":
		s, ok := v.(string)
		return ok && strings.TrimSpace(s) != ""
	case "gender":
		n, ok := auditNum(v)
		return ok && (n == 1 || n == 2)
	default: // "num": JSON-number counters (explicit zero binds; non-integers don't)
		n, ok := auditNum(v)
		return ok && n >= 0 && n == float64(int64(n))
	}
}

// auditNum accepts the JSON-decoder number shapes we scan.
func auditNum(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	default:
		return 0, false
	}
}

type fieldStat struct {
	Field    string  `json:"field"`
	Complete int64   `json:"complete"`
	Total    int64   `json:"total"`
	Pct      float64 `json:"pct"`
}

type auditResult struct {
	StoreDir    string      `json:"store_dir"`
	Collection  string      `json:"collection"`
	RowsScanned int64       `json:"rows_scanned"`
	Authors     int64       `json:"authors"`
	Fields      []fieldStat `json:"fields"`
	OverallPct  float64     `json:"overall_pct"`
	MinPct      float64     `json:"min_pct"`
	Pass        bool        `json:"pass"`
}

// auditCollectionRe validates collection names like store does.
var auditCollectionRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// scanCommentsAudit streams <store>/<collection>.jsonl and computes the
// 12-field completeness ledger. Missing collection fails closed (a typo'd
// --store must never yield a vacuous pass); a malformed row names its line.
func scanCommentsAudit(storeDir, collection string) (*auditResult, error) {
	if strings.TrimSpace(storeDir) == "" {
		return nil, errors.New("audit-comments: --store is required")
	}
	if collection == "" {
		collection = "comments"
	}
	if !auditCollectionRe.MatchString(collection) {
		return nil, fmt.Errorf("audit-comments: invalid collection %q", collection)
	}
	file := filepath.Join(storeDir, collection+".jsonl")
	fh, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("audit-comments: collection not found: %s (store %s)", file, storeDir)
		}
		return nil, fmt.Errorf("audit-comments: open: %w", err)
	}
	defer fh.Close()

	res := &auditResult{StoreDir: storeDir, Collection: collection,
		Fields: make([]fieldStat, len(auditFields))}
	for i, af := range auditFields {
		res.Fields[i] = fieldStat{Field: af.Name}
	}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	lineNo := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		lineNo++
		if line == "" {
			continue
		}
		res.RowsScanned++
		var row map[string]any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&row); err != nil {
			return nil, fmt.Errorf("audit-comments: %s line %d: %w", filepath.Base(file), lineNo, err)
		}
		user, _ := row["user"].(map[string]any)
		if user == nil {
			user = map[string]any{} // all fields incomplete for this author
		}
		res.Authors++
		for i, af := range auditFields {
			res.Fields[i].Total++
			if fieldComplete(af, user[af.Name]) {
				res.Fields[i].Complete++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("audit-comments: scan %s: %w", file, err)
	}
	finalizeAudit(res)
	return res, nil
}

// finalizeAudit computes per-field and overall percentages plus the AC-19
// pass decision at the configured floor.
func finalizeAudit(res *auditResult) {
	if res.MinPct <= 0 {
		res.MinPct = defaultMinCompleteness
	}
	var sum float64
	for i := range res.Fields {
		fs := &res.Fields[i]
		if fs.Total > 0 {
			fs.Pct = round1(float64(fs.Complete) / float64(fs.Total) * 100)
		}
		sum += fs.Pct
	}
	if len(res.Fields) > 0 {
		res.OverallPct = round1(sum / float64(len(res.Fields)))
	}
	res.Pass = res.Authors > 0 && res.RowsScanned > 0 && res.OverallPct >= res.MinPct
}

const defaultMinCompleteness = 90.0

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

type auditOpts struct {
	storeDir   string
	collection string
	minPct     float64
	out        string
}

func cmdLabAuditComments(args []string) error {
	o := auditOpts{}
	fs := flag.NewFlagSet("lab audit-comments", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "use: lab audit-comments --store <dir> [--collection comments] [--min-pct 90] [--out file.json]\n")
	}
	fs.StringVar(&o.storeDir, "store", "", "JSONL store directory to audit")
	fs.StringVar(&o.collection, "collection", "comments", "collection name (default comments)")
	fs.Float64Var(&o.minPct, "min-pct", defaultMinCompleteness, "AC-19 completeness floor percent")
	fs.StringVar(&o.out, "out", "", "also write the JSON report to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := scanCommentsAudit(o.storeDir, o.collection)
	if err != nil {
		return err
	}
	res.MinPct = o.minPct
	finalizeAudit(res)
	raw, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	if o.out != "" {
		if err := os.WriteFile(o.out, append(raw, '\n'), 0o644); err != nil {
			return fmt.Errorf("audit-comments: write out: %w", err)
		}
	}
	if !res.Pass {
		return fmt.Errorf("audit-comments: overall completeness %.1f%% below floor %.1f%% (%d authors)",
			res.OverallPct, res.MinPct, res.Authors)
	}
	return nil
}
