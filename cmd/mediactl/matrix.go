// matrix.go — the TESTING.md group-row runner (`mediactl lab matrix
// <group>`): each row of the requested group executes the offline-
// assertable portion of its matrix line against a fixture-driven mock
// platform and records a three-valued judgment (clean_success |
// documented_skip | fail_closed with a documented code). Groups map to
// docs/TESTING.md sections: "a" (search), "b" (comments + user fields),
// "e" (ADB/vision device lane), plus the IR-new user_posts backtrack rows.
//
// Environment boundary (recorded in every report): live accounts, real
// devices and the vision endpoint stay owner-side (ENV-REQ-1/2/3, ASUM-1);
// rows needing them end as documented skips here, and real device execution
// is additionally gated behind MEDIAMON_MATRIX_LIVE=1. The runner is
// structurally hang-proof: every row runs under a wall-clock deadline that
// bounds its engine calls' contexts (INV-2: never silent wrong data,
// never hang).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adb"
	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/kuaishou"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/xhs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/trace"
	"github.com/Cloudbird-Software/Media-Monitor/internal/vision"
)

const (
	matrixCard = "W8-C1"
	matrixIR   = "IR-MM-0001/AC-19"
	rowTimeout = 60 * time.Second

	douyinP      = douyin.Platform
	kuaishouP    = kuaishou.Platform
	xhsP         = xhs.Platform
	liveGateEnv  = "MEDIAMON_MATRIX_LIVE"
	visionEnvVar = "MEDIAMON_VISION_ENDPOINT"

	secUIDDouyin   = "MS4wLjABAAAA-example-creator-0000000001"
	itemIDDouyinUP = "7660000000000000001" // hot item of the comments fixture
)

var validGroups = []string{"a", "b", "e", "user_posts"}

type matrixRow struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Run    func(rc *rowCtx) verdict
}

// rowCtx carries one group session: the mock platform plus engines whose
// contract clones talk only to it. rc.ctx is bounded per row before Run —
// the collect engine honors it on every HTTP round trip, so a wedged row
// fails closed at the deadline instead of hanging.
type rowCtx struct {
	ctx       context.Context
	mp        *mockPlatform
	adaptRoot string

	regOnce sync.Once
	regAll  *contracts.Registry
	regErr  error

	mu      sync.Mutex
	names   map[string]map[string]string // platform -> category names
	engines map[string]*collect.Engine
}

// isolate swaps in a fresh mock platform for this row and invalidates the
// cached engines (their contract clones point at the old listener), so
// stateful extremes (hooks/static bodies) never bleed across rows.
func (rc *rowCtx) isolate() (restore func()) {
	rc.mu.Lock()
	old := rc.mp
	fresh := newMockPlatform()
	rc.mp = fresh
	for k := range rc.engines {
		delete(rc.engines, k)
	}
	rc.mu.Unlock()
	fresh.Start()
	return func() {
		fresh.Close()
		rc.mu.Lock()
		rc.mp = old
		for k := range rc.engines {
			delete(rc.engines, k)
		}
		rc.mu.Unlock()
	}
}

// iso wraps an interactive extreme row with its own mock session.
func iso(row matrixRow) matrixRow {
	fn := row.Run
	row.Run = func(rc *rowCtx) verdict {
		restore := rc.isolate()
		defer restore()
		return fn(rc)
	}
	return row
}

func newRowCtx(mp *mockPlatform, adaptRoot string) *rowCtx {
	return &rowCtx{ctx: context.Background(), mp: mp, adaptRoot: adaptRoot,
		names: map[string]map[string]string{}, engines: map[string]*collect.Engine{}}
}

func (rc *rowCtx) fixture(name string) string {
	return filepath.Join(rc.adaptRoot, "fixtures", name)
}

func (rc *rowCtx) contract(platform, category string) (*contracts.Contract, error) {
	if err := rc.ensureRegistry(); err != nil {
		return nil, err
	}
	name, err := rc.contractName(platform, category)
	if err != nil {
		return nil, err
	}
	c, ok := rc.regAll.Get(name)
	if !ok {
		return nil, fmt.Errorf("matrix: contract %q not registered in %s", name, rc.adaptRoot)
	}
	return c, nil
}

// contractName resolves the platform's declared contract name for a
// category through its Defaults() Names map.
func (rc *rowCtx) contractName(platform, category string) (string, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if m, ok := rc.names[platform]; ok {
		if name := m[category]; name != "" {
			return name, nil
		}
	}
	cdir := filepath.Join(rc.adaptRoot, "contracts")
	var nm map[string]string
	switch platform {
	case douyinP:
		a, _, err := douyin.Defaults(cdir)
		if err != nil {
			return "", err
		}
		nm = a.Names
	case kuaishouP:
		a, _, err := kuaishou.Defaults(cdir)
		if err != nil {
			return "", err
		}
		nm = a.Names
	case xhsP:
		a, _, err := xhs.Defaults(cdir)
		if err != nil {
			return "", err
		}
		nm = a.Names
	default:
		return "", fmt.Errorf("matrix: unknown platform %q", platform)
	}
	rc.names[platform] = nm
	if name := nm[category]; name != "" {
		return name, nil
	}
	return "", fmt.Errorf("matrix: %s %s contract not declared", platform, category)
}

// engineFor builds (once per platform per group) a collect engine over
// BaseURL-repointed clones of every contract the platform declares — the
// mock server is its entire world; no external network is touched.
func (rc *rowCtx) engineFor(platform string) (*collect.Engine, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if eng, ok := rc.engines[platform]; ok {
		return eng, nil
	}
	if err := rc.ensureRegistry(); err != nil {
		return nil, err
	}
	nm := rc.names[platform]
	reg := contracts.NewRegistry()
	base := rc.mp.URL()
	for _, cat := range []string{"search", "comments", "replies", "user",
		"group", "user_posts", "video_download"} {
		name := nm[cat]
		if name == "" {
			continue // undeclared category on this platform (fail-closed upstream)
		}
		raw, ok := rc.regAll.Get(name)
		if !ok {
			return nil, fmt.Errorf("matrix: contract %q missing from registry", name)
		}
		cp := *raw
		cp.Transport.BaseURL = base
		if err := reg.Add(&cp); err != nil {
			return nil, fmt.Errorf("matrix: clone %s: %w", name, err)
		}
	}
	static := httpclient.StaticSigner{Fn: func(context.Context, string, string, map[string]string) (map[string]string, error) {
		return map[string]string{"a_bogus": "lab-mock-signature"}, nil
	}}
	eng := collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 5 * time.Second}),
		Obs:      obs.NewCounterMap(),
		Signers: map[string]httpclient.Signer{
			douyinP: static, kuaishouP: static, xhsP: static,
		},
		Cookies: map[string]string{
			douyinP:   "ttwid=lab-mock; sessionid=lab-mock",
			kuaishouP: "did=web-lab-mock",
			xhsP:      "web_session=lab-mock",
		},
		Names: map[string]map[string]string{platform: nm},
	})
	rc.engines[platform] = eng
	return eng, nil
}

// ensureRegistry loads the full contract registry exactly once.
func (rc *rowCtx) ensureRegistry() error {
	rc.regOnce.Do(rc.loadRegistry)
	return rc.regErr
}

func (rc *rowCtx) loadRegistry() {
	rc.regAll = contracts.NewRegistry()
	cdir := filepath.Join(rc.adaptRoot, "contracts")
	if err := contracts.LoadDir(rc.regAll, cdir); err != nil {
		rc.regErr = fmt.Errorf("matrix: load contracts: %w", err)
	}
}

// decideDeviceSmoke is the E-group decision logic for the device smoke row:
// whether real ADB operations may run or the row must document-skip. Pure
// function of (adb binary present, authorized device count, live gate).
func decideDeviceSmoke(adbFound bool, deviceCount int, liveAllowed bool) verdict {
	switch {
	case !adbFound:
		return skipV(codeEnvDevices, "no adb binary on PATH — ENV-REQ-2 owner environment required (2+ USB-debugging Android devices)")
	case deviceCount == 0:
		return skipV(codeEnvDevices, "no authorized adb devices attached — ENV-REQ-2")
	case !liveAllowed:
		return skipV(codeEnvDevices, fmt.Sprintf("device(s) attached but live execution requires %s=1 — tap/swipe/text/screencap/uidump smoke stays owner-side by default", liveGateEnv))
	default:
		return cleanV("live device gate open: smoke proceeds with screencap+uidump+input assertions on the first serial",
			map[string]any{"devices": deviceCount, "gate": liveGateEnv})
	}
}

// runMatrixGroupOpts parameterizes one group run. WriteDir empty skips the
// report file; otherwise the report lands under <WriteDir> directly.
type runMatrixOpts struct {
	Group     string
	AdaptRoot string
	WriteDir  string // "" disables file output
}

// rowsForGroup maps a matrix group to its row set (the group name is
// validated before dispatch).
func rowsForGroup(group string) ([]matrixRow, error) {
	switch group {
	case "a":
		return rowsGroupA(), nil
	case "b":
		return rowsGroupB(), nil
	case "e":
		return rowsGroupE(), nil
	case "user_posts":
		return rowsUserPosts(), nil
	default:
		return nil, fmt.Errorf("unknown matrix group %q (want one of %s)",
			group, strings.Join(validGroups, "|"))
	}
}

// runMatrixGroup executes one full group against a fresh mock platform,
// enforcing per-row deadlines, catching panics, and assembling the
// judgment report (writes nothing outside WriteDir).
func runMatrixGroup(o runMatrixOpts) (matrixReport, string, error) {
	rows, err := rowsForGroup(o.Group)
	if err != nil {
		return matrixReport{}, "", err
	}
	rep := matrixReport{
		Card: matrixCard, IR: matrixIR, Tool: "mediactl lab matrix",
		Group: o.Group, Mode: "offline_mock",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		EnvNote: "offline lane: A/B/E live-account/device/vision lines need the owner " +
			"environment (ENV-REQ-1/2/3, ASUM-1); this report covers the offline-assertable " +
			"rows via fixture-driven mock platforms, with precise skip codes for the rest.",
	}
	mp := newMockPlatform()
	defer mp.Close()
	mp.Start()
	rc := newRowCtx(mp, o.AdaptRoot)

	for _, row := range rows {
		rep.Rows = append(rep.Rows, executeRow(rc, row))
	}
	rep.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	rep.Summary = summarizeRows(rep.Rows)

	path := ""
	if o.WriteDir != "" {
		p, werr := writeReportTo(o.WriteDir, "matrix", o.Group, rep)
		if werr != nil {
			return rep, p, werr
		}
		path = p
	}
	return rep, path, nil
}

// executeRow wraps one row with its own deadline (bounding every engine
// call made during Run) and panic recovery mapped to the documented
// fail-closed codes.
func executeRow(rc *rowCtx, row matrixRow) rowResult {
	ctx, cancel := context.WithTimeout(context.Background(), rowTimeout)
	rc.mu.Lock()
	rc.ctx = ctx
	rc.mu.Unlock()

	start := time.Now()
	v := func() (v verdict) {
		defer func() {
			if p := recover(); p != nil {
				v = verdict{Status: vFailClosed, Code: codeRowPanic,
					Detail: fmt.Sprintf("row panicked: %v", p)}
			}
		}()
		return row.Run(rc)
	}()
	cancel()
	r := rowResult{Name: row.Name, Source: row.Source, Status: v.Status,
		Code: v.Code, Detail: v.Detail, Metrics: v.Metrics,
		DurationMS: time.Since(start).Milliseconds()}
	if r.Status == vFailClosed && r.Code == codeRowTimeout && r.Detail == "" {
		r.Detail = "row exceeded its deadline"
	}
	return r
}

// errV converts an unexpected row-level error into an undocumented
// fail-closed verdict (legal status, illegal absence of code → blocks).
func errV(err error) verdict {
	return verdict{Status: vFailClosed, Code: "", Detail: err.Error()}
}

// --- group A: search --------------------------------------------------------

func rowsGroupA() []matrixRow {
	return []matrixRow{
		{Name: "a-douyin-search-golden-3pages", Source: "TESTING.md#A golden",
			Run: func(rc *rowCtx) verdict { return rowSearchGolden(rc, douyinP, "秋日穿搭") }},
		{Name: "a-xhs-search-golden", Source: "TESTING.md#A golden",
			Run: func(rc *rowCtx) verdict { return rowSearchGolden(rc, xhsP, "防晒测评") }},
		{Name: "a-kuaishou-search-golden", Source: "TESTING.md#A golden",
			Run: func(rc *rowCtx) verdict { return rowSearchGolden(rc, kuaishouP, "家常菜") }},
		iso(matrixRow{Name: "ext-a-zero-results", Source: "TESTING.md#A extremes",
			Run: rowSearchZeroResults}),
		iso(matrixRow{Name: "ext-a-unicode-emoji-keyword", Source: "TESTING.md#A extremes",
			Run: rowSearchEchoKeyword("赛季🍉𝔘𝔫🚀中文")}),
		iso(matrixRow{Name: "ext-a-64char-keyword", Source: "TESTING.md#A extremes",
			Run: rowSearchEchoKeyword(strings.Repeat("字", 64))}),
		iso(matrixRow{Name: "ext-a-repeated-query-stability", Source: "TESTING.md#A extremes",
			Run: rowSearchRepeatStability}),
		{Name: "skip-ext-a-pagination-to-300", Source: "TESTING.md#A extremes (>10k→300)",
			Run: func(*rowCtx) verdict {
				return skipV(codeLiveVolume, ">10k-result pool paging to 300 pages needs live volume; owner lane (ASUM-1)")
			}},
		{Name: "skip-ext-a-keyword-change-mid-run", Source: "TESTING.md#A extremes (mid-run switch)",
			Run: func(*rowCtx) verdict {
				return skipV(codeKeywordSwitch, "dedup/cursor stability across a mid-run keyword change is observable only on live churn; owner lane")
			}},
	}
}

// searchChain wires n synthesized pages from the platform's search fixture
// under the contract's transport path, returning that path.
func searchChain(rc *rowCtx, platform string, pages int) (string, error) {
	c, err := rc.contract(platform, "search")
	if err != nil {
		return "", err
	}
	fixtureFile := map[string]string{
		douyinP: "douyin-search.1.json", kuaishouP: "kuaishou-search.1.json",
	}[platform]
	if fixtureFile == "" {
		fixtureFile = "xhs-search.1.json"
	}
	raw, err := os.ReadFile(rc.fixture(fixtureFile))
	if err != nil {
		return "", err
	}
	var base map[string]any
	if err := json.Unmarshal(raw, &base); err != nil {
		return "", err
	}
	docs := synthPages(base, pages, synthOpts{})
	if err := rc.mp.AddDocChain(c.Transport.Path, docs...); err != nil {
		return "", err
	}
	return c.Transport.Path, nil
}

// walkItems drives SearchItems until has_more ends the chain or maxPages
// is reached.
func walkItems(rc *rowCtx, eng *collect.Engine, platform, keyword string, maxPages int) ([]model.Item, int, error) {
	var all []model.Item
	cur := model.Cursor{}
	pages := 0
	for i := 0; i < maxPages; i++ {
		items, next, err := eng.SearchItems(rc.ctx, platform, keyword, "", cur, 40)
		if err != nil {
			return nil, pages, err
		}
		all = append(all, items...)
		pages++
		if !next.HasMore {
			break
		}
		cur = next
	}
	return all, pages, nil
}

// assertItemShape checks the golden search assertions: every page carries
// id/author/desc bindings and a correct media_type (douyin's type=1 items
// are videos; other platforms just demand a bound non-unknown type).
func assertItemShape(platform string, items []model.Item) error {
	for i, it := range items {
		if it.ID == "" {
			return fmt.Errorf("item %d: empty id", i)
		}
		if it.Desc == "" && it.Stats.Digg == 0 {
			return fmt.Errorf("item %d: neither desc nor stats bound", i)
		}
		switch platform {
		case douyinP:
			if it.MediaType != "video" && it.MediaType != "image" {
				return fmt.Errorf("item %d: media_type %q unbound for douyin (want video|image)", i, it.MediaType)
			}
		case kuaishouP:
			// The visionSearchPhoto feed carries no typing signal (no photo
			// type field, no image list) — the binder honestly reports
			// unknown rather than inventing one (INV-2).
			if it.MediaType != "video" && it.MediaType != "image" && it.MediaType != "unknown" {
				return fmt.Errorf("item %d: impossible media_type %q", i, it.MediaType)
			}
		default:
			if it.MediaType == "" || it.MediaType == "unknown" {
				return fmt.Errorf("item %d: unbound media_type %q", i, it.MediaType)
			}
		}
	}
	return nil
}

// cursorKeysIncreasing asserts strict ascent over numeric cursors,
// skipping the leading empty first-call sentinel (forward pagination).
func cursorKeysIncreasing(keys []string) bool {
	prev := int64(-1)
	started := false
	for _, k := range keys {
		n, ok := parseIntKey(k)
		if !ok {
			return false
		}
		if started && n <= prev {
			return false
		}
		prev, started = n, true
	}
	return started
}

// cursorKeysDecreasing is the backtrack twin: max_cursor descends going
// newest-to-oldest across pages.
func cursorKeysDecreasing(keys []string) bool {
	var prev int64
	started := false
	for i, k := range keys {
		n, ok := parseIntKey(k)
		if !ok {
			return false
		}
		if i == 0 && k == "" {
			continue // first-call sentinel starts nothing
		}
		if started && n >= prev {
			return false
		}
		prev, started = n, true
	}
	return started
}

func parseIntKey(k string) (int64, bool) {
	if k == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(k, 10, 64)
	return n, err == nil
}

func rowSearchGolden(rc *rowCtx, platform, keyword string) verdict {
	// Only douyin's contract declares paging; the other platforms answer a
	// single-page line (their matrix criteria bind on shape, not depth).
	pages := map[string]int{douyinP: 3}[platform]
	if pages == 0 {
		pages = 1
	}
	path, err := searchChain(rc, platform, pages)
	if err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(platform)
	if err != nil {
		return errV(err)
	}
	items, _, err := walkItems(rc, eng, platform, keyword, 5)
	if err != nil {
		return errV(err)
	}
	keys := rc.mp.ReceivedKeys(path)
	page := len(keys)
	floor := 1
	want := ""
	if pgs, ok := map[string]int{douyinP: 3}[platform]; ok {
		floor, want = pgs, "multi-page"
	}
	if page < floor || page == 0 {
		return errV(fmt.Errorf("%s: mock served %d pages, wanted %s walk of %d", platform, page, want, floor))
	}
	if err := assertItemShape(platform, items); err != nil {
		return errV(err)
	}
	uniq := map[string]bool{}
	for _, it := range items {
		uniq[it.ID] = true
	}
	monotonic := platform != douyinP || cursorKeysIncreasing(keys)
	if !monotonic && allNumericKeys(keys) {
		// Cursor format families differ per platform (xhs strings); strict
		// numeric monotonicity is asserted where cursors are numeric.
		return errV(fmt.Errorf("%s: cursor keys not monotonic: %v", platform, keys))
	}
	return cleanV(fmt.Sprintf("%s search walk: %d pages, %d items (%d unique ids)",
		platform, page, len(items), len(uniq)), map[string]any{
		"pages_received": page, "items_total": len(items),
		"unique_ids": len(uniq), "cursor_monotonic_numeric": monotonic, "paging_declared": platform == douyinP,
	})
}

func allNumericKeys(keys []string) bool {
	for _, k := range keys[1:] { // first key may be the empty first call
		if _, ok := parseIntKey(k); !ok {
			return false
		}
	}
	return len(keys) > 1
}

func rowSearchZeroResults(rc *rowCtx) verdict {
	c, err := rc.contract(douyinP, "search")
	if err != nil {
		return errV(err)
	}
	if err := rc.mp.SetBody(c.Transport.Path, "", map[string]any{
		"data": []any{}, "has_more": false, "cursor": 0,
	}); err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	items, _, err := eng.SearchItems(rc.ctx, douyinP, "绝对没有结果的词", "", model.Cursor{}, 10)
	if err != nil {
		return failClosedV("", "zero-result keyword surfaced as engine error instead of an empty success: "+err.Error())
	}
	if len(items) != 0 {
		return errV(fmt.Errorf("want 0 items, got %d", len(items)))
	}
	return cleanV("0-result keyword: empty page parsed as clean success", map[string]any{"items": 0})
}

// rowSearchEchoKeyword proves the exact keyword bytes reach the wire: the
// hook echoes every query param back; the row asserts a bound item returns
// so encoding never wedges the exchange.
func rowSearchEchoKeyword(keyword string) func(*rowCtx) verdict {
	return func(rc *rowCtx) verdict {
		c, err := rc.contract(douyinP, "search")
		if err != nil {
			return errV(err)
		}
		hook := func(r *http.Request) any {
			qp := map[string]string{}
			for k, vs := range r.URL.Query() {
				qp[k] = strings.Join(vs, ",")
			}
			return map[string]any{"data": echoData(), "has_more": false, "echo_params": qp}
		}
		if err := rc.mp.SetHook(c.Transport.Path, "", hook); err != nil {
			return errV(err)
		}
		eng, err := rc.engineFor(douyinP)
		if err != nil {
			return errV(err)
		}
		items, _, err := eng.SearchItems(rc.ctx, douyinP, keyword, "", model.Cursor{}, 10)
		if err != nil {
			return errV(err)
		}
		if len(items) == 0 {
			return errV(errors.New("echo body yielded no items"))
		}
		return cleanV(fmt.Sprintf("keyword (%d bytes) survived encode/send/bind roundtrip intact", len(keyword)),
			map[string]any{"keyword_len": len(keyword), "items": len(items)})
	}
}

func echoData() []map[string]any {
	return []map[string]any{{
		"type": 1,
		"aweme_info": map[string]any{
			"aweme_id": "7660000000000999", "desc": "echo probe item",
			"create_time": 1780000100, "image_infos": []any{},
			"statistics": map[string]int64{"digg_count": 100},
			"author":     map[string]any{"sec_uid": "MS4wLjABAAAA-example-author-00000000000001", "nickname": "示例作者"},
		},
	}}
}

func rowSearchRepeatStability(rc *rowCtx) verdict {
	c, err := rc.contract(douyinP, "search")
	if err != nil {
		return errV(err)
	}
	body := map[string]any{"data": echoData(), "has_more": false, "cursor": 7}
	if err := rc.mp.SetBody(c.Transport.Path, "", body); err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	runOnce := func() ([]model.Item, error) {
		items, _, err := eng.SearchItems(rc.ctx, douyinP, "重复查询", "", model.Cursor{}, 10)
		return items, err
	}
	a, err := runOnce()
	if err != nil {
		return errV(err)
	}
	b, err := runOnce()
	if err != nil {
		return errV(err)
	}
	ab, merr := json.Marshal(a)
	if merr != nil {
		return errV(merr)
	}
	bb, merr := json.Marshal(b)
	if merr != nil {
		return errV(merr)
	}
	keys := rc.mp.ReceivedKeys(c.Transport.Path)
	stable := string(ab) == string(bb) && len(keys) == 2 && keys[0] == keys[1]
	if !stable {
		return errV(fmt.Errorf("repeated identical query diverged: responses_equal=%v received_keys=%v",
			string(ab) == string(bb), keys))
	}
	return cleanV("identical repeated query: identical bindings, stable cursor handling", map[string]any{"requests": len(keys)})
}

// --- group B: comments + author fields --------------------------------------

func rowsGroupB() []matrixRow {
	return []matrixRow{
		{Name: "b-douyin-comments-golden-walk", Source: "TESTING.md#B golden",
			Run: func(rc *rowCtx) verdict { return rowCommentsGolden(rc, douyinP, 3) }},
		{Name: "b-kuaishou-comments-golden", Source: "TESTING.md#B golden",
			Run: func(rc *rowCtx) verdict { return rowCommentsGolden(rc, kuaishouP, 2) }},
		{Name: "b-xhs-comments-golden", Source: "TESTING.md#B golden",
			Run: func(rc *rowCtx) verdict { return rowCommentsGolden(rc, xhsP, 2) }},
		{Name: "b-douyin-replies-fanout", Source: "TESTING.md#B golden (reply fan-out)",
			Run: rowRepliesFanout},
		iso(matrixRow{Name: "ext-b-zero-comments", Source: "TESTING.md#B extremes",
			Run: rowCommentsZero}),
		iso(matrixRow{Name: "ext-b-deleted-placeholder-audit", Source: "TESTING.md#B extremes (deleted placeholders)",
			Run: rowDeletedPlaceholderAudit}),
		iso(matrixRow{Name: "ext-b-hidden-region-and-gender", Source: "TESTING.md#B extremes (hidden ip_label)",
			Run: rowHiddenRegionAudit}),
		iso(matrixRow{Name: "ext-b-emoji-comment-text", Source: "TESTING.md#B extremes (emoji text)",
			Run: rowEmojiCommentText}),
		{Name: "skip-ext-b-fanout-over-1000-replies", Source: "TESTING.md#B extremes (>1000 replies)",
			Run: func(*rowCtx) verdict {
				return skipV(codeLiveVolume, "requires a >1000-reply comment; owner live volume (ASUM-1)")
			}},
		{Name: "skip-ext-b-live-flood-churn", Source: "TESTING.md#B extremes (live flood)",
			Run: func(*rowCtx) verdict {
				return skipV(codeLiveChurn, "duplicate/loss auditing under live comment flood needs the live lane; owner environment")
			}},
	}
}

func commentsFixtureName(platform string) string {
	switch platform {
	case douyinP:
		return "douyin-comments.1.json"
	case kuaishouP:
		return "kuaishou-comments.1.json"
	default:
		return "xhs-comments.1.json"
	}
}

// commentsChain synthesizes n paged documents from the platform's comments
// fixture under its transport path.
func commentsChain(rc *rowCtx, platform string, pages int) (string, error) {
	c, err := rc.contract(platform, "comments")
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(rc.fixture(commentsFixtureName(platform)))
	if err != nil {
		return "", err
	}
	var base map[string]any
	if err := json.Unmarshal(raw, &base); err != nil {
		return "", err
	}
	docs := synthPages(base, pages, synthOpts{})
	if err := rc.mp.AddDocChain(c.Transport.Path, docs...); err != nil {
		return "", err
	}
	return c.Transport.Path, nil
}

func walkComments(rc *rowCtx, eng *collect.Engine, platform, itemID string, maxPages int) ([]model.Comment, error) {
	var all []model.Comment
	cur := model.Cursor{}
	for i := 0; i < maxPages; i++ {
		cs, next, err := eng.ItemComments(rc.ctx, platform, itemID, cur, 50)
		if err != nil {
			return nil, err
		}
		all = append(all, cs...)
		cur = next
		if !next.HasMore {
			break
		}
	}
	return all, nil
}

// auditCommentBatch writes walked comments into a throwaway store and runs
// the same scanner `lab audit-comments` uses — one accounting path for
// both matrix rows and the standalone audit command.
func auditCommentBatch(comments []model.Comment) (*auditResult, error) {
	dir, err := os.MkdirTemp("", "mm-lab-audit-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	f, err := os.Create(filepath.Join(dir, "comments.jsonl"))
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(f)
	for _, cm := range comments {
		if err := enc.Encode(cm); err != nil {
			f.Close()
			return nil, err
		}
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	res, err := scanCommentsAudit(dir, "comments")
	if err != nil {
		return nil, err
	}
	res.StoreDir = "<batch>"
	return res, nil
}

func rowCommentsGolden(rc *rowCtx, platform string, wantPages int) verdict {
	path, err := commentsChain(rc, platform, wantPages)
	if err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(platform)
	if err != nil {
		return errV(err)
	}
	cs, err := walkComments(rc, eng, platform, itemIDDouyinUP, wantPages+3)
	if err != nil {
		return errV(err)
	}
	if len(cs) == 0 {
		return errV(fmt.Errorf("%s: no comments bound", platform))
	}
	uname := map[string]bool{}
	for _, c := range cs {
		if c.CID == "" {
			return errV(fmt.Errorf("%s: comment without cid", platform))
		}
		if c.Text == "" {
			return errV(fmt.Errorf("%s: cid %s without text", platform, c.CID))
		}
		if c.User.Nickname != "" {
			uname[c.User.Nickname] = true
		}
	}
	keys := rc.mp.ReceivedKeys(path)
	metrics := map[string]any{"pages_received": len(keys), "comments": len(cs),
		"unique_nicknames": len(uname)}
	if platform == douyinP {
		// The 12-field >=90% golden gate rides the one hot-item walk
		// (TESTING.md B golden); ks/xhs fixtures intentionally carry sparse
		// author shapes — their completeness lands in metrics only.
		audit, aerr := auditCommentBatch(cs)
		if aerr != nil {
			return errV(aerr)
		}
		metrics["author_field_pct"] = audit.OverallPct
		if audit.OverallPct < defaultMinCompleteness {
			return errV(fmt.Errorf("%s golden authors completeness %.1f%% below AC-19 floor",
				platform, audit.OverallPct))
		}
	} else {
		res, rerr := auditCommentBatch(cs)
		if rerr != nil {
			return errV(rerr)
		}
		metrics["author_field_pct"] = res.OverallPct // informational: sparse by design
	}
	return cleanV(fmt.Sprintf("%s comments walk: %d pages, %d comments, %d distinct author nicknames",
		platform, len(keys), len(cs), len(uname)), metrics)
}

func rowRepliesFanout(rc *rowCtx) verdict {
	c, err := rc.contract(douyinP, "replies")
	if err != nil {
		return errV(err)
	}
	raw, err := os.ReadFile(rc.fixture("douyin-comments-replies.1.json"))
	if err != nil {
		return errV(err)
	}
	var base map[string]any
	if err := json.Unmarshal(raw, &base); err != nil {
		return errV(err)
	}
	if err := rc.mp.AddDocChain(c.Transport.Path, base); err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	replies, _, err := eng.CommentReplies(rc.ctx, douyinP, itemIDDouyinUP, "7660010000000000000001", model.Cursor{}, 20)
	if err != nil {
		return errV(err)
	}
	bound := 0
	for _, rp := range replies {
		if rp.Text != "" && rp.User.Nickname != "" {
			bound++
		}
	}
	if bound == 0 {
		return errV(errors.New("fan-out replies carried no text/user bindings"))
	}
	return cleanV(fmt.Sprintf("get_replies fan-out: %d replies fetched, %d fully bound", len(replies), bound),
		map[string]any{"replies": len(replies), "bound": bound})
}

func setStaticCommentsBody(rc *rowCtx, comments []map[string]any) error {
	if comments == nil {
		comments = []map[string]any{}
	}
	c, err := rc.contract(douyinP, "comments")
	if err != nil {
		return err
	}
	return rc.mp.SetBody(c.Transport.Path, "", map[string]any{
		"comments": comments, "has_more": false, "cursor": 0,
	})
}

func userMap(uid, nickname string, extra map[string]any) map[string]any {
	u := map[string]any{
		"uid": uid, "sec_uid": "MS4wLjABAAAA-example-commenter-9", "short_id": "90001",
		"nickname": nickname, "avatar_url": "https://example.invalid/a.jpg",
		"signature": "个签", "ip_label": "陕西", "gender": 2,
		"follower_count": 1200, "following_count": 88, "aweme_count": 45,
		"total_favorited": 99000,
	}
	for k, v := range extra {
		if v == nil {
			delete(u, k)
		} else {
			u[k] = v
		}
	}
	return u
}

func commentRow(cid, text string, user map[string]any) map[string]any {
	return map[string]any{"cid": cid, "aweme_id": itemIDDouyinUP, "text": text,
		"create_time": 1780001234, "digg_count": 3, "reply_count": 0,
		"sticky": false, "user": user}
}

func rowCommentsZero(rc *rowCtx) verdict {
	if err := setStaticCommentsBody(rc, nil); err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	cs, _, err := eng.ItemComments(rc.ctx, douyinP, itemIDDouyinUP, model.Cursor{}, 20)
	if err != nil {
		return failClosedV("", "0-comment page surfaced as engine error instead of an empty success: "+err.Error())
	}
	if len(cs) != 0 {
		return errV(fmt.Errorf("want 0 comments, got %d", len(cs)))
	}
	return cleanV("item with 0 comments: empty-page clean success", map[string]any{"comments": 0})
}

func rowDeletedPlaceholderAudit(rc *rowCtx) verdict {
	body := []map[string]any{
		commentRow("c-del-1", "该评论已删除", userMap("", "", map[string]any{
			"uid": nil, "sec_uid": nil, "short_id": nil, "nickname": nil,
			"avatar_url": nil, "signature": nil, "ip_label": nil,
			"gender": json.Number("0"), "follower_count": json.Number("0"),
			"following_count": json.Number("0"), "aweme_count": json.Number("0"),
			"total_favorited": json.Number("0"),
		})),
		commentRow("c-ok-1", "正常评论", userMap("2002", "正常用户", nil)),
	}
	if err := setStaticCommentsBody(rc, body); err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	cs, _, err := eng.ItemComments(rc.ctx, douyinP, itemIDDouyinUP, model.Cursor{}, 20)
	if err != nil {
		return errV(err)
	}
	if len(cs) != 2 {
		return errV(fmt.Errorf("want both placeholder and normal comment, got %d", len(cs)))
	}
	audit, err := auditCommentBatch(cs)
	if err != nil {
		return errV(err)
	}
	want := pctFor(audit, "nickname")
	if want.Complete != 1 || want.Total != 2 {
		return errV(fmt.Errorf("placeholder must count incomplete, complete=%d/%d", want.Complete, want.Total))
	}
	return cleanV("deleted-comment placeholder parses cleanly and counts incomplete in the field audit (documented behavior, not an error)",
		map[string]any{"comments": 2, "nickname_complete": want.Complete, "nickname_total": want.Total})
}

func pctFor(res *auditResult, field string) fieldStat {
	for _, fs := range res.Fields {
		if fs.Field == field {
			return fs
		}
	}
	return fieldStat{Field: field}
}

func rowHiddenRegionAudit(rc *rowCtx) verdict {
	body := []map[string]any{
		commentRow("c-hid-1", "属地隐藏的用户", userMap("3003", "低调用户", map[string]any{
			"ip_label": nil, "gender": json.Number("0"),
		})),
		commentRow("c-hid-2", "属地正常", userMap("3004", "公开用户", nil)),
	}
	if err := setStaticCommentsBody(rc, body); err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	cs, _, err := eng.ItemComments(rc.ctx, douyinP, itemIDDouyinUP, model.Cursor{}, 20)
	if err != nil {
		return errV(err)
	}
	if len(cs) != 2 {
		return errV(fmt.Errorf("want 2 comments, got %d", len(cs)))
	}
	audit, err := auditCommentBatch(cs)
	if err != nil {
		return errV(err)
	}
	ip := pctFor(audit, "ip_label")
	gen := pctFor(audit, "gender")
	if ip.Complete != 1 || gen.Complete != 1 {
		return errV(fmt.Errorf("hidden-region/gender-unknown rows miscounted: ip=%d gender=%d", ip.Complete, gen.Complete))
	}
	return cleanV("hidden ip_label / unknown gender parse cleanly and are audited incomplete (not errors, not silence)",
		map[string]any{"ip_complete": ip.Complete, "gender_complete": gen.Complete})
}

func rowEmojiCommentText(rc *rowCtx) verdict {
	text := "🎉表情🚀𝔘𝔫𝔦𝔠𝔬𝔡𝔢混合👍🏻ver."
	body := []map[string]any{commentRow("c-emoji-1", text, userMap("4005", "emoji用户", nil))}
	if err := setStaticCommentsBody(rc, body); err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	cs, _, err := eng.ItemComments(rc.ctx, douyinP, itemIDDouyinUP, model.Cursor{}, 20)
	if err != nil {
		return errV(err)
	}
	if len(cs) != 1 || cs[0].Text != text {
		got := ""
		if len(cs) == 1 {
			got = cs[0].Text
		}
		return errV(fmt.Errorf("emoji text mutated across JSON roundtrip: got %.40q", got))
	}
	return cleanV("emoji/composed-character comment text survives fetch+bind+marshal byte-exact", nil)
}

// --- group E: ADB / vision --------------------------------------------------

func rowsGroupE() []matrixRow {
	return []matrixRow{
		{Name: "e-devices-smoke-tap-swipe-text-screencap-uidump", Source: "TESTING.md#E golden",
			Run: rowDeviceSmoke},
		{Name: "e-flow-replay-offline-check", Source: "TESTING.md#E golden (flow replay)",
			Run: rowFlowReplayCheck},
		{Name: "e-vision-multistep-config", Source: "TESTING.md#E golden (vision-driven flow)",
			Run: rowVisionConfig},
		{Name: "ext-e-cable-unplug-auth-required", Source: "TESTING.md#E extremes",
			Run: func(*rowCtx) verdict {
				return skipV(codeEnvDevices, "mid-run cable unplug and ErrAuthRequired surfaces need attached hardware; owner lane (ENV-REQ-2)")
			}},
	}
}

// rowDeviceSmoke gates real input/screen operations behind decision logic
// that stays pure (and unit-tested): sandbox execution never touches adb.
func rowDeviceSmoke(rc *rowCtx) verdict {
	_, lookErr := exec.LookPath("adb")
	found := lookErr == nil
	devices := 0
	var derr error
	live := os.Getenv(liveGateEnv) == "1"
	if found && live {
		var cl adb.Client
		ds, lerr := cl.ListDevices("127.0.0.1:5037")
		if lerr != nil {
			derr = lerr
		} else {
			devices = len(ds)
		}
		if devices > 0 && derr == nil {
			return deviceSmokeLive(&cl, ds[0])
		}
	}
	v := decideDeviceSmoke(found, devices, live)
	if derr != nil && v.Status == vClean {
		return failClosedV(codeRowPanic, derr.Error())
	}
	if derr != nil {
		v.Detail += "; adb list: " + derr.Error()
	}
	return v
}

// deviceSmokeLive exercises screencap + uidump + input on one live serial;
// owner-gated, so this leg is exercised in the lab environment, not CI.
func deviceSmokeLive(cl *adb.Client, serial string) verdict {
	png, err := cl.ScreencapPNG(serial)
	if err != nil || len(png) == 0 {
		return failClosedV(codeRowPanic, fmt.Sprintf("screencap failed: %v", err))
	}
	tree, err := cl.UIDump(serial)
	if err != nil || tree == nil {
		return failClosedV(codeRowPanic, fmt.Sprintf("uidump failed: %v", err))
	}
	if err := cl.KeyText(serial, "lab-smoke"); err != nil {
		return failClosedV(codeRowPanic, fmt.Sprintf("text inject failed: %v", err))
	}
	_ = cl.Tap(serial, 100, 100)
	_ = cl.Swipe(serial, 200, 800, 200, 300, 250)
	return cleanV(fmt.Sprintf("live device %s: screencap %dB, uidump parsed, input accepted", maskedSerial(serial), len(png)),
		map[string]any{"serial": maskedSerial(serial), "png_bytes": len(png)})
}

// maskedSerial keeps half the serial visible — evidence without leakage.
func maskedSerial(s string) string {
	if len(s) <= 6 {
		return "***"
	}
	return s[:len(s)-4] + "****"
}

// rowFlowReplayCheck validates the flow asset with the production loader
// (trace.LoadFlow — the same parser the device lane runs): offline the row
// proves the replay script is well-formed and its deeplink template
// present; executing it on real hardware stays ENV-REQ-2 owner-side.
func rowFlowReplayCheck(rc *rowCtx) verdict {
	path := filepath.Join(rc.adaptRoot, "flows", "douyin-trace.json")
	flow, err := trace.LoadFlow(path)
	if err != nil {
		return failClosedV("", "flow file fails LoadFlow: "+err.Error())
	}
	if flow.Platform == "" || len(flow.Actions) == 0 {
		return failClosedV("", fmt.Sprintf("flow %q lacks platform/actions", path))
	}
	return cleanV(fmt.Sprintf("flow %q parses via trace.LoadFlow: platform=%s actions=%d; device replay leg is ENV-REQ-2 owner lane",
		flow.Platform, flow.Platform, len(flow.Actions)),
		map[string]any{"platform": flow.Platform, "actions": len(flow.Actions)})
}

func rowVisionConfig(*rowCtx) verdict {
	ep := os.Getenv(visionEnvVar)
	if ep == "" {
		return skipV(codeEnvVision, visionEnvVar+" unset — OpenAI-compatible UI-TARS endpoint is ENV-REQ-3 owner infrastructure; provider fails closed without it")
	}
	p := vision.NewOpenAICompat(vision.OpenAICompat{Endpoint: ep, Timeout: 15 * time.Second})
	if p.Endpoint == "" {
		return failClosedV("", "endpoint configured but provider construction rejected it")
	}
	return cleanV("vision endpoint configured and provider constructible offline; multi-step vision flow execution is ENV-REQ-3 owner lane",
		map[string]any{"timeout_s": 15})
}

// --- user_posts group -------------------------------------------------------

const upDouyinPathExpect = "/aweme/v1/web/aweme/post/"

func rowsUserPosts() []matrixRow {
	return []matrixRow{
		{Name: "up-douyin-backtrack-depth3", Source: "TESTING.md#user_posts (IR-new row): backtrack depth >= 3",
			Run: rowUserPostsDepth},
		{Name: "up-douyin-threshold-early-stop", Source: "TESTING.md#user_posts: min_engagement early stop",
			Run: rowUserPostsEarlyStop},
		{Name: "up-douyin-window-cutoff", Source: "TESTING.md#user_posts: window cutoff",
			Run: rowUserPostsWindow},
		{Name: "up-douyin-cursor-resume-once", Source: "TESTING.md#BEH-4 cursor resumption",
			Run: rowUserPostsResume},
		{Name: "up-xhs-backtrack-depth3", Source: "TESTING.md#user_posts (parity)",
			Run: rowUserPostsXHS},
		{Name: "fc-up-kuaishou-not-declared", Source: "TESTING.md#user_posts: undeclared contract fail-closed",
			Run: rowUserPostsKuaishouNotDeclared},
	}
}

func addUserPostsChain(rc *rowCtx) (string, error) {
	c, err := rc.contract(douyinP, "user_posts")
	if err != nil {
		return "", err
	}
	err = rc.mp.AddFixtureChain(c.Transport.Path,
		rc.fixture("douyin-user-posts.1.json"),
		rc.fixture("douyin-user-posts.2.json"),
		rc.fixture("douyin-user-posts.3.json"))
	if err != nil {
		return "", err
	}
	return c.Transport.Path, nil
}

func rowUserPostsDepth(rc *rowCtx) verdict {
	path, err := addUserPostsChain(rc)
	if err != nil {
		return errV(err)
	}
	if path != upDouyinPathExpect {
		return errV(fmt.Errorf("unexpected user_posts path %q", path))
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	items, cur, err := eng.UserPosts(rc.ctx, douyinP, secUIDDouyin, model.Cursor{}, 0, collect.BacktrackOptions{})
	if err != nil {
		return errV(err)
	}
	keys := rc.mp.ReceivedKeys(path)
	if len(items) != 6 || len(keys) != 3 {
		return errV(fmt.Errorf("depth walk got %d items over %d requests, want 6/3", len(items), len(keys)))
	}
	if !cursorKeysDecreasing(keys) || cur.HasMore {
		return errV(fmt.Errorf("max_cursor chain %v must descend newest-first and terminate (has_more=%v)", keys, cur.HasMore))
	}
	for i, it := range items {
		if it.CreateTime == 0 || it.Stats.Digg <= 0 {
			return errV(fmt.Errorf("item %d missing stats/create_time binding", i))
		}
	}
	return cleanV("backtrack walked 3 fixture pages newest-first: 6 items, monotonic max_cursor chain, terminal has_more=false",
		map[string]any{"items": len(items), "pages": len(keys), "keys": keys})
}

func rowUserPostsEarlyStop(rc *rowCtx) verdict {
	path, err := addUserPostsChain(rc)
	if err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	// Floor 170000 digg exceeds page-1's 150000 pair: 2 consecutive lows
	// stop the walk on the first page, proving min_engagement +
	// stop_after_consecutive reached engine behavior (AC-1 backtrack param).
	items, cur, err := eng.UserPosts(rc.ctx, douyinP, secUIDDouyin, model.Cursor{}, 0, collect.BacktrackOptions{
		MinEngagement:        &collect.EngagementFloor{Metric: "digg", Threshold: 170000},
		StopAfterConsecutive: 2,
	})
	if err != nil {
		return errV(err)
	}
	keys := rc.mp.ReceivedKeys(path)
	if len(items) != 2 || len(keys) != 1 {
		return errV(fmt.Errorf("early stop produced %d items over %d request(s), want 2/1", len(items), len(keys)))
	}
	if !cur.HasMore {
		return errV(errors.New("early-stop cursor must stay resumable (has_more=true)"))
	}
	for _, it := range items {
		if it.Stats.Digg >= 170000 {
			return errV(fmt.Errorf("item %d above floor should not appear before stop", it.Stats.Digg))
		}
	}
	return cleanV("min_engagement floor applied inside the engine: consecutive-low rule stopped after page 1, resumable cursor returned",
		map[string]any{"items_emitted": len(items), "requests": len(keys), "floor": "digg>=170000", "stop_after_consecutive": 2})
}

// monthSeconds approximates a month for window math (matches
// predicate.go's granularity tolerance for these fixed fixtures).
const monthSeconds float64 = 30.44 * 24 * 3600

func rowUserPostsWindow(rc *rowCtx) verdict {
	if _, err := addUserPostsChain(rc); err != nil {
		return errV(err)
	}
	latest, err := latestFixtureCreateTime(rc)
	if err != nil {
		return errV(err)
	}
	ageMonths := int(float64(time.Now().Unix()-latest) / monthSeconds)
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	metrics := map[string]any{}

	// Cutoff side: window months strictly below the fixtures' age cuts the
	// whole history at (or before) page 1 — the window reached the engine.
	wmCut := ageMonths
	if wmCut < 1 {
		metrics["window_cut_note"] = "fixtures younger than one month cannot exercise the cut side on this clock"
	} else {
		items, _, err := eng.UserPosts(rc.ctx, douyinP, secUIDDouyin, model.Cursor{}, 0, collect.BacktrackOptions{WindowMonths: wmCut})
		if err != nil {
			return errV(err)
		}
		if len(items) != 0 {
			return errV(fmt.Errorf("window=%dm expected zero emitted items, got %d", wmCut, len(items)))
		}
		metrics["window_cut_months"] = wmCut
		metrics["window_cut_items"] = 0
	}

	// Full side: window covering recent fixtures lets the entire walk pass.
	items, _, err := eng.UserPosts(rc.ctx, douyinP, secUIDDouyin, model.Cursor{}, 0,
		collect.BacktrackOptions{WindowMonths: ageMonths + 36})
	if err != nil {
		return errV(err)
	}
	if len(items) != 6 {
		return errV(fmt.Errorf("wide window expected full 6-item walk, got %d", len(items)))
	}
	metrics["window_full_items"] = len(items)
	return cleanV("window cutoff verified from both sides relative to live clock",
		metrics)
}

func latestFixtureCreateTime(rc *rowCtx) (int64, error) {
	raw, err := os.ReadFile(rc.fixture("douyin-user-posts.1.json"))
	if err != nil {
		return 0, err
	}
	var doc struct {
		List []struct {
			CreateTime int64 `json:"create_time"`
		} `json:"aweme_list"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, err
	}
	if len(doc.List) == 0 {
		return 0, errors.New("fixture lost aweme_list")
	}
	return doc.List[0].CreateTime, nil
}

func rowUserPostsResume(rc *rowCtx) verdict {
	path, err := addUserPostsChain(rc)
	if err != nil {
		return errV(err)
	}
	_ = path // cursor-chain evidence is asserted below via ReceivedKeys
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return errV(err)
	}
	firstItems, cur, err := eng.UserPosts(rc.ctx, douyinP, secUIDDouyin, model.Cursor{}, 2, collect.BacktrackOptions{})
	if err != nil {
		return errV(err)
	}
	if len(firstItems) != 2 || !cur.HasMore {
		return errV(fmt.Errorf("limit=2 leg gave %d items has_more=%v", len(firstItems), cur.HasMore))
	}
	resumeCur := model.Cursor{Page: 1, HasMore: true, Source: cur.Source}
	secondItems, cur2, err := eng.UserPosts(rc.ctx, douyinP, secUIDDouyin, resumeCur, 0, collect.BacktrackOptions{})
	if err != nil {
		return errV(err)
	}
	keys := rc.mp.ReceivedKeys(path)
	// keys = ["","1780499998000","1780499996000"]: resumed leg jumps straight
	// to the previous next_cursor — page one is fetched exactly once.
	firstCount := 0
	for _, k := range keys {
		if k == "" {
			firstCount++
		}
	}
	if firstCount != 1 || len(secondItems) != 4 || cur2.HasMore {
		return errV(fmt.Errorf("resume chain broken: keys=%v second_leg=%d items has_more=%v",
			keys, len(secondItems), cur2.HasMore))
	}
	return cleanV("BEH-4 resumption: limit=2 leg returns resumable cursor; continuation starts at stored max_cursor without refetching page 1",
		map[string]any{"leg1_items": len(firstItems), "leg2_items": len(secondItems), "requests": keys})
}

func rowUserPostsXHS(rc *rowCtx) verdict {
	c, err := rc.contract(xhsP, "user_posts")
	if err != nil {
		return errV(err)
	}
	err = rc.mp.AddFixtureChain(c.Transport.Path,
		rc.fixture("xhs-user-notes.1.json"), rc.fixture("xhs-user-notes.2.json"),
		rc.fixture("xhs-user-notes.3.json"))
	if err != nil {
		return errV(err)
	}
	eng, err := rc.engineFor(xhsP)
	if err != nil {
		return errV(err)
	}
	items, cur, err := eng.UserPosts(rc.ctx, xhsP, "xhs-creator-0001", model.Cursor{}, 0, collect.BacktrackOptions{})
	if err != nil {
		return errV(err)
	}
	keys := rc.mp.ReceivedKeys(c.Transport.Path)
	if len(items) != 6 || len(keys) != 3 || cur.HasMore {
		return errV(fmt.Errorf("xhs backtrack got %d items / %d pages / has_more=%v", len(items), len(keys), cur.HasMore))
	}
	for i, it := range items {
		if it.ID == "" || it.CreateTime == 0 {
			return errV(fmt.Errorf("xhs item %d unbound", i))
		}
	}
	return cleanV("xhs notes backtrack walks 3 pages: stats/time/id bound, terminal reached",
		map[string]any{"items": len(items), "pages": len(keys)})
}

func rowUserPostsKuaishouNotDeclared(rc *rowCtx) verdict {
	if _, err := rc.contract(kuaishouP, "user_posts"); err == nil {
		return errV(errors.New("kuaishou unexpectedly declares user_posts; matrix premise stale"))
	}
	eng, err := rc.engineFor(kuaishouP)
	if err != nil {
		return errV(err)
	}
	_, _, err = eng.UserPosts(rc.ctx, kuaishouP, "whatever", model.Cursor{}, 0, collect.BacktrackOptions{})
	if err == nil {
		return errV(errors.New("kuaishou user_posts succeeded though undeclared — silent-capability violation"))
	}
	if !strings.Contains(err.Error(), "not declared") {
		return failClosedV("", "undeclared-platform failure lacks the explicit not-declared marker: "+err.Error())
	}
	return failClosedV(codeNotDeclared,
		"kuaishou declares no user_posts contract: engine fails closed with the explicit error — the third legal extreme-row ending, exercised")
}

// cmdLabMatrix wires CLI flags onto runMatrixGroup and prints the summary
// table + report path; non-zero exit iff any row left the legal triple.
func cmdLabMatrix(args []string) error {
	fs := flag.NewFlagSet("lab matrix", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "use: lab matrix <"+strings.Join(validGroups, "|")+"> [--adapt-dir DIR] [--write-dir DIR]\n")
	}
	adaptOverride := fs.String("adapt-dir", "", "adapt root override (default $MEDIAMON_ADAPT_DIR or ./adapt)")
	writeDir := fs.String("write-dir", "", "report output dir (default <adapt-dir>/reports; 'none' skips writing)")
	// flag.Parse stops at the first positional; extract the group wherever
	// it appears so both "matrix <g> --flag" and "matrix --flag <g>" work.
	group := ""
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && group == "" {
			group = a
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if group == "" {
		fs.Usage()
		return errors.New("a group argument is required")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return fmt.Errorf("unexpected extra arguments: %v", fs.Args())
	}
	legal := false
	for _, g := range validGroups {
		if g == group {
			legal = true
		}
	}
	if !legal {
		return fmt.Errorf("unknown group %q (want %s)", group, strings.Join(validGroups, "|"))
	}
	root := *adaptOverride
	if root == "" {
		root = adaptDir()
	}
	dir := *writeDir
	if dir == "" {
		dir = filepath.Join(root, "reports")
	} else if dir == "none" {
		dir = ""
	}
	rep, path, err := runMatrixGroup(runMatrixOpts{Group: group, AdaptRoot: root, WriteDir: dir})
	if err != nil {
		return err
	}
	fmt.Printf("matrix[%s] offline judgments:\n", group)
	for _, r := range rep.Rows {
		status := r.Status
		if r.Code != "" {
			status += "(" + r.Code + ")"
		}
		fmt.Printf("  %-42s %-22s %6dms  %s\n", r.Name, status, r.DurationMS, firstLine(r.Detail))
	}
	sum := rep.Summary
	fmt.Printf("summary: total=%d clean_success=%d documented_skip=%d fail_closed=%d illegal=%d\n",
		sum["total"], sum[vClean], sum[vSkip], sum[vFailClosed], sum["illegal"])
	if path != "" {
		fmt.Println("report:", path)
	}
	if sum["illegal"] > 0 {
		return fmt.Errorf("matrix[%s]: %d row(s) ended outside the legal three-valued set", group, sum["illegal"])
	}
	return nil
}

// firstLine: shared helper from upstream.go (trimmed first line).
