// search_stream.go — 真站搜索第一页链（2026-09-06 真站实证）。
//
// 真站 douyin 关键词搜索的第一页首选 /aweme/v1/web/general/search/stream/：
// 响应是非标准 chunk 帧内嵌的 NDJSON 文档流，帧内 extra 的 logid 需作为
// search_id 参数回传给后续 single 翻页。single 直发在多数会话状态下静默回空
// （语料 R5A 与真站双证）。引擎按契约类别 "search_stream" 自动先走本链，
// 失败回落原 single 行为（合成站无 stream 端点时保持全兼容）。
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// decodeStreamDocs parses douyin's stream body: non-standard chunk framing
// (bare hex-size lines) wrapping concatenated NDJSON documents. Robust to
// CDP-style mid-document chunk splits by dropping standalone hex/CRLF lines
// before a raw_decode loop; zero docs is a clean empty (caller falls back).
func decodeStreamDocs(body []byte) []map[string]any {
	var sb strings.Builder
	for _, ln := range strings.Split(string(body), "\n") {
		t := strings.TrimRight(ln, "\r")
		if t == "" || isHexFrameSize(t) {
			continue
		}
		sb.WriteString(t)
	}
	dec := json.NewDecoder(strings.NewReader(sb.String()))
	docs := make([]map[string]any, 0, 4)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		docs = append(docs, m)
	}
	return docs
}

// isHexFrameSize reports whether the whole line is a bare chunk-size token
// (1-8 hex digits), e.g. "a88b4".
func isHexFrameSize(t string) bool {
	if len(t) == 0 || len(t) > 8 {
		return false
	}
	for _, r := range t {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// FetchStreamPage fetches one stream-contract page and returns the merged
// pseudo-document (data concatenated across frames; has_more/cursor/extra
// from the last frame) plus the logid for the search_id continuation chain.
func (e *Engine) FetchStreamPage(ctx context.Context, name string, pathParams, query map[string]string) (doc map[string]any, logid string, err error) {
	c, ok := e.reg.Get(name)
	if !ok {
		return nil, "", fmt.Errorf("collect: contract %q not registered", name)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if e.obs != nil {
		e.obs.Inc("collect.fetch", 1)
	}
	full, headers, body, err := e.buildURL(ctx, c, pathParams, query)
	if err != nil {
		e.fetchErr()
		return nil, "", err
	}
	_, proxy, _, _ := e.accountContext(c.Platform)
	hc := e.fetchClient(c.Platform, proxy)
	status, resp, err := hc.WithContract(name).Do(ctx, c.Transport.Method, full, headers, body)
	if err != nil {
		e.fetchErr()
		return nil, "", err
	}
	if status < 200 || status >= 300 {
		e.fetchErr()
		return nil, "", fmt.Errorf("collect %s: status %d", name, status)
	}
	docs := decodeStreamDocs(resp)
	if len(docs) == 0 {
		return nil, "", fmt.Errorf("collect %s: empty stream", name)
	}
	last := docs[len(docs)-1]
	merged := map[string]any{"data": []any{}}
	n := 0
	for _, d := range docs {
		if arr, ok := d["data"].([]any); ok {
			ary := merged["data"].([]any)
			ary = append(ary, arr...)
			merged["data"] = ary
			n += len(arr)
		}
	}
	for _, k := range []string{"has_more", "cursor", "extra", "log_pb"} {
		if v, ok := last[k]; ok {
			merged[k] = v
		}
	}
	// logid for the search_id chain: extra.log_pb.logid (corpus R5A form),
	// with flat fallbacks.
	if ex, ok := last["extra"].(map[string]any); ok {
		if lp, ok := ex["log_pb"].(map[string]any); ok {
			if v, ok := lp["logid"].(string); ok {
				logid = v
			}
		}
		if logid == "" {
			if v, ok := ex["logid"].(string); ok {
				logid = v
			}
		}
	}
	if e.obs != nil {
		e.obs.Inc("collect.stream_frames", int64(len(docs)))
	}
	_ = n
	return merged, logid, nil
}

// applyMediaFilter post-filters items by MediaType (search filter face).
func applyMediaFilter(items []model.Item, filter string) []model.Item {
	if filter == "" {
		return items
	}
	f := items[:0]
	for _, it := range items {
		if it.MediaType == filter {
			f = append(f, it)
		}
	}
	return f
}

// searchFirstPageStream walks page 1 of a douyin keyword search through the
// stream contract. Returns the bound items, the continuation cursor (with
// search_id stashed) and ok=false when the stream face is unavailable (caller
// falls back to the single-contract walk).
func (e *Engine) searchFirstPageStream(ctx context.Context, streamName string, keyword, filter string, limit int) ([]model.Item, model.Cursor, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	sc, ok := e.reg.Get(streamName)
	if !ok {
		return nil, model.Cursor{}, false
	}
	doc, logid, err := e.FetchStreamPage(ctx, streamName, map[string]string{"keyword": keyword}, map[string]string{})
	if err != nil {
		return nil, model.Cursor{}, false
	}
	bp, err := contracts.ParsePath(sc.Binding.Items)
	if err != nil {
		return nil, model.Cursor{}, false
	}
	recs := selectRecords(bp, doc)
	items := make([]model.Item, 0, len(recs))
	for _, r := range recs {
		items = append(items, bindItem(sc, r))
	}
	if filter != "" {
		f := items[:0]
		for _, it := range items {
			if it.MediaType == filter {
				f = append(f, it)
			}
		}
		items = f
	}
	nxt := e.nextCursor(sc, doc, model.Cursor{})
	if logid != "" {
		if nxt.Source == nil {
			nxt.Source = map[string]any{}
		}
		nxt.Source["search_id"] = logid
	}
	nxt.Page = 1
	return items, nxt, true
}
