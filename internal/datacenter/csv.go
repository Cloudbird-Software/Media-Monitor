package datacenter

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// csvHeader is the stable column order for CSV export.
var csvHeader = []string{"platform", "user_key", "nickname", "avatar_url", "timestamp", "payload"}

// WriteCSV writes records as CSV to w, optionally filtered by keywords.
func WriteCSV(w io.Writer, records []Record, keywords []string, matchAny bool) error {
	out := csv.NewWriter(w)
	defer out.Flush()
	if err := out.Write(csvHeader); err != nil {
		return fmt.Errorf("datacenter: csv header: %w", err)
	}
	// newest first
	sorted := make([]Record, 0, len(records))
	for _, r := range records {
		if len(keywords) > 0 && !matchKeywords(r, keywords, matchAny) {
			continue
		}
		sorted = append(sorted, r)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Timestamp > sorted[j].Timestamp })
	for _, r := range sorted {
		payload := ""
		if r.Payload != nil {
			b, err := compactJSON(r.Payload)
			if err == nil {
				payload = b
			}
		}
		row := []string{r.Platform, r.UserKey, r.Nickname, r.AvatarURL, fmtInt(r.Timestamp), payload}
		if err := out.Write(row); err != nil {
			return fmt.Errorf("datacenter: csv row: %w", err)
		}
	}
	return nil
}

// compactJSON marshals v to a single-line string (best-effort).
func compactJSON(v any) (string, error) {
	b, err := marshalCompact(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func marshalCompact(v any) ([]byte, error) {
	return jsonMarshal(v)
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func fmtInt(n int64) string {
	return fmt.Sprintf("%d", n)
}

var _ = strings.Join
var _ = sort.Slice
