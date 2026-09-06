// batchdetail_test.go — BatchDetails unit tests: chunking under the
// count-clamp discipline, silent omission of unknown ids, JSON-array-string
// body slot, dedup of the caller's id list, fail-closed rows.
package collect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

func batchDetailContract(srv *httptest.Server) *contracts.Contract {
	return &contracts.Contract{
		Name: "mock-multi-detail", Platform: "mock", Category: "multi_detail", Version: "1",
		Transport: contracts.Transport{
			BaseURL: srv.URL, Path: "/multi/detail", Method: http.MethodPost,
			Query: map[string]string{"aweme_ids": ""},
		},
		Binding: contracts.Binding{Items: "$.aweme_details"},
	}
}

// batchDetailServer records the per-request id payloads and answers with the
// subset of requested ids that exist (unknown ids silently omitted, the
// endpoint's own semantics).
func batchDetailServer(t *testing.T, known map[string]bool) (*httptest.Server, *[]string, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var raws []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		_ = json.Unmarshal(body, &doc)
		raw, _ := doc["aweme_ids"].(string)
		var ids []string
		_ = json.Unmarshal([]byte(raw), &ids)
		mu.Lock()
		raws = append(raws, raw)
		mu.Unlock()
		out := []map[string]any{}
		for _, id := range ids {
			if !known[id] {
				continue // silently omitted
			}
			out = append(out, map[string]any{
				"aweme_id":   id,
				"desc":       "batch item " + id,
				"statistics": map[string]any{"digg_count": 100 + len(id)},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"aweme_details": out, "status_code": 0})
	}))
	t.Cleanup(srv.Close)
	return srv, &raws, &mu
}

func batchDetailEngine(t *testing.T, c *contracts.Contract) *Engine {
	t.Helper()
	return mockEngine(t, addContracts(t, c), func(ctx *Context) {
		ctx.Pacing = &PacingConfig{}
		ctx.Names = map[string]map[string]string{c.Platform: {"multi_detail": c.Name}}
	})
}

func batchIDs(prefix string, n int) []string {
	out := make([]string, 0, n)
	letters := "abcdefghijklmnopqrstuvwxyz0123456789"
	for i := 0; i < n; i++ {
		out = append(out, "70000000000000"+prefix+string(letters[i%36])+string(rune('0'+i/36)))
	}
	return out
}

func TestBatchDetailsChunkingAndClamp(t *testing.T) {
	known := map[string]bool{}
	ids := batchIDs("aa", 45)
	for _, id := range ids {
		known[id] = true
	}
	srv, raws, mu := batchDetailServer(t, known)
	eng := batchDetailEngine(t, batchDetailContract(srv))
	res, err := eng.BatchDetails(context.Background(), "mock", ids, BatchDetailOptions{})
	if err != nil {
		t.Fatalf("BatchDetails: %v", err)
	}
	if res.Requested != 45 || res.Returned != 45 || len(res.Missing) != 0 {
		t.Fatalf("outcome wrong: %+v", res)
	}
	if res.Batches != 3 {
		t.Fatalf("batches = %d, want 3 (20/20/5)", res.Batches)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*raws) != 3 {
		t.Fatalf("requests = %d, want 3", len(*raws))
	}
	for i, raw := range *raws {
		var got []string
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("request %d body slot is not a JSON-array string: %q", i, raw)
		}
		want := 20
		if i == 2 {
			want = 5
		}
		if len(got) != want {
			t.Fatalf("request %d ids = %d, want %d (count-clamp batch discipline)", i, len(got), want)
		}
	}
	if res.Items[0].ID != ids[0] || res.Items[0].Stats.Digg != int64(100+len(ids[0])) {
		t.Fatalf("binding wrong: %+v", res.Items[0])
	}
}

func TestBatchDetailsHardCapAndLowerOverride(t *testing.T) {
	known := map[string]bool{}
	ids := batchIDs("bb", 30)
	for _, id := range ids {
		known[id] = true
	}
	// MaxPerBatch above the endpoint ceiling clamps down to 20.
	srv, raws, mu := batchDetailServer(t, known)
	eng := batchDetailEngine(t, batchDetailContract(srv))
	res, err := eng.BatchDetails(context.Background(), "mock", ids, BatchDetailOptions{MaxPerBatch: 50})
	if err != nil {
		t.Fatalf("BatchDetails: %v", err)
	}
	if res.Batches != 2 || res.Returned != 30 {
		t.Fatalf("hard cap ignored: %+v", res)
	}
	mu.Lock()
	if len(*raws) != 2 {
		t.Fatalf("requests = %d, want 2", len(*raws))
	}
	mu.Unlock()
	// A lower caller cap wins (chunking more aggressively).
	srv2, raws2, mu2 := batchDetailServer(t, known)
	eng2 := batchDetailEngine(t, batchDetailContract(srv2))
	res2, err := eng2.BatchDetails(context.Background(), "mock", ids, BatchDetailOptions{MaxPerBatch: 8})
	if err != nil {
		t.Fatalf("BatchDetails(lower cap): %v", err)
	}
	if res2.Batches != 4 || res2.Returned != 30 {
		t.Fatalf("lower cap ignored: %+v", res2)
	}
	mu2.Lock()
	defer mu2.Unlock()
	if len(*raws2) != 4 {
		t.Fatalf("requests = %d, want 4", len(*raws2))
	}
}

func TestBatchDetailsUnknownIdsSilentlyOmitted(t *testing.T) {
	known := map[string]bool{"7000000000000000001": true, "7000000000000000002": true}
	srv, _, _ := batchDetailServer(t, known)
	eng := batchDetailEngine(t, batchDetailContract(srv))
	res, err := eng.BatchDetails(context.Background(), "mock",
		[]string{"7000000000000000001", "9999999999999999999", "7000000000000000002"},
		BatchDetailOptions{})
	if err != nil {
		t.Fatalf("BatchDetails: %v", err)
	}
	if res.Returned != 2 || len(res.Missing) != 1 || res.Missing[0] != "9999999999999999999" {
		t.Fatalf("silent omission not reported: %+v", res)
	}
	if len(res.Items) != 2 || res.Items[0].ID != "7000000000000000001" {
		t.Fatalf("bound items wrong: %+v", res.Items)
	}
}

func TestBatchDetailsDedupAndFailClosedRows(t *testing.T) {
	known := map[string]bool{"7000000000000000001": true}
	srv, _, _ := batchDetailServer(t, known)
	eng := batchDetailEngine(t, batchDetailContract(srv))
	res, err := eng.BatchDetails(context.Background(), "mock",
		[]string{"7000000000000000001", "7000000000000000001", " "}, BatchDetailOptions{})
	if err != nil {
		t.Fatalf("BatchDetails: %v", err)
	}
	if res.Requested != 1 || res.Returned != 1 {
		t.Fatalf("dedup wrong: %+v", res)
	}
	if _, err := eng.BatchDetails(context.Background(), "mock", nil, BatchDetailOptions{}); err == nil || !strings.Contains(err.Error(), "no item ids") {
		t.Fatalf("empty id list must fail closed, got %v", err)
	}
	// Platform without a multi_detail contract: not declared, fail closed.
	_, err = eng.BatchDetails(context.Background(), "nosuch", []string{"1"}, BatchDetailOptions{})
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared contract must fail closed, got %v", err)
	}
}
