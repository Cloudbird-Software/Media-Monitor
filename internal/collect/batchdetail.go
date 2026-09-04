// batchdetail.go — BatchDetails, the batch-detail enrichment atom
// (capability F / proposals G, P2): given a list of item ids, resolve their
// details through the platform's multi-detail face — one request per ≤20-id
// batch instead of N single-shot detail fetches (the metric-refresh scenario
// drops request volume ~20x, serving the silence goal directly).
//
// Design notes (atom boundary / schema trade-offs):
//   - Batching follows the count-clamp discipline: the per-batch size derives
//     from the global per-request cap (DefaultMaxCountPerRequest = 20,
//     MEDIAMON_MAX_COUNT) and is hard-clamped to the endpoint's own batch
//     ceiling of 20 (corpus/f2 evidence); the caller may only lower it via
//     BatchDetailOptions.MaxPerBatch. A 45-id call is 3 requests (20/20/5).
//   - The aweme_ids value is a JSON-array STRING riding the contract's empty
//     query slot (the corpus form body encodes exactly this string shape; the
//     engine's POST body carries the same value — see the contract doc).
//   - Unknown ids are SILENTLY OMITTED by the endpoint (probe evidence: no
//     partial failure); the atom reports them in Missing instead of guessing
//     an error, and per-batch fetch failures keep already-collected items
//     (partial-data semantics, like every walk atom).
//   - Batch/single field consistency is a caller-level concern (the e2e
//     target asserts aweme_id + digg_count equality against the single-shot
//     detail face); the atom binds through the standard item binder only.
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// BatchDetail is the outcome of one batch-detail enrichment call.
type BatchDetail struct {
	// Requested is the count of unique ids handed in (duplicates dropped).
	Requested int `json:"requested"`
	// Returned is the count of detail records bound.
	Returned int `json:"returned"`
	// Missing lists ids the endpoint silently omitted (unknown ids).
	Missing []string `json:"missing,omitempty"`
	// Batches is the number of requests issued.
	Batches int `json:"batches"`
	// Items are the bound detail records (full item shape, stats included).
	Items []model.Item `json:"items"`
}

// BatchDetailOptions tunes the batching policy.
type BatchDetailOptions struct {
	// MaxPerBatch caps ids per request; 0 = the per-request count cap
	// (MEDIAMON_MAX_COUNT, default 20). Values above the endpoint's batch
	// ceiling are clamped down to it.
	MaxPerBatch int
}

// batchDetailHardCap is the multi/aweme/detail endpoint's own batch ceiling
// (corpus/f2: 20 ids per request; the synth oracle truncates beyond silently,
// the real site rejects) — the atom never asks for more per request.
const batchDetailHardCap = 20

// resolveBatchSize clamps the requested batch size: caller cap (optional) →
// global per-request count cap → the endpoint hard ceiling, floor 1.
func resolveBatchSize(requested int) int {
	size := requested
	if size <= 0 {
		size = maxCountPerRequest()
	}
	if size > batchDetailHardCap {
		size = batchDetailHardCap
	}
	if size < 1 {
		size = 1
	}
	return size
}

// BatchDetails resolves the details of many item ids through the platform's
// multi-detail contract: one request per ≤20-id batch, unknown ids reported
// in Missing, every returned record bound through the standard item binder.
// Fails closed on an empty id list or an undeclared multi_detail contract.
func (e *Engine) BatchDetails(ctx context.Context, platform string, ids []string, opt BatchDetailOptions) (BatchDetail, error) {
	var out BatchDetail
	seen := map[string]bool{}
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return out, fmt.Errorf("collect: batch detail: no item ids provided")
	}
	name, err := e.resolveName(platform, "multi_detail")
	if err != nil {
		return out, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return out, fmt.Errorf("collect: contract %q not registered", name)
	}
	_, raw := mainBindingRaw(c)
	if raw == "" {
		return out, fmt.Errorf("collect %s: no list binding declared for batch details", name)
	}
	bp, err := contracts.ParsePath(raw)
	if err != nil {
		return out, err
	}
	slot := querySlotParam(c)
	size := resolveBatchSize(opt.MaxPerBatch)
	paging := pacingFor(e.pacing, c.Paging.PageSleepMS)
	out.Requested = len(uniq)
	for start := 0; start < len(uniq); start += size {
		if ctx.Err() != nil {
			break
		}
		end := start + size
		if end > len(uniq) {
			end = len(uniq)
		}
		batch := uniq[start:end]
		if out.Batches > 0 {
			e.pageThink(ctx, paging)
		}
		payload, merr := json.Marshal(batch)
		if merr != nil {
			return out, merr
		}
		doc, ferr := e.Fetch(ctx, name, nil, map[string]string{slot: string(payload)})
		if ferr != nil {
			// Partial-data semantics: keep the batches already collected.
			return out, ferr
		}
		out.Batches++
		got := map[string]bool{}
		for _, rec := range selectRecords(bp, doc) {
			it := bindItem(c, rec)
			if it.ID == "" {
				continue
			}
			got[it.ID] = true
			out.Items = append(out.Items, it)
		}
		for _, id := range batch {
			if !got[id] {
				out.Missing = append(out.Missing, id)
			}
		}
		e.obsInc("collect.batch_detail", 1)
	}
	out.Returned = len(out.Items)
	return out, nil
}
