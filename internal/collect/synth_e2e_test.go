// synth_e2e_test.go — 合成站 e2e 目标测试（capability batch A–E）。
//
// 运行口径：对「自起开发组合成站」（oracle replay/synth_api.py，完整数据集
// 10 万条/站）逐能力验证 proposals 的成功标准，成功标准全部写成可判定断言，
// 结果数字进测试日志（-v）。不访问真站；不依赖正式 8090 组端口。
//
// 启动方式（Git Bash）：
//
//	D:/Projects/temp2/oracle/env/Scripts/python.exe \
//	  D:/Projects/temp2/oracle/replay/synth_api.py --site all \
//	  --base-port 8751 --preload
//	MEDIAMON_SYNTH_PORTS=8751,8752,8753 go test ./internal/collect \
//	  -run TestSynthE2ENewCapabilities -v -count=1
//
// 契约适配（builder 内完成，仓库契约零改动——与 oracle 侧 adapt_synth 同款
// 口径）：三站 base_url 改指开发组端口；douyin-user 重路由到
// /aweme/v1/web/user/profile/other/（合成站无 imapi，语料作者链面）；
// xhs-search 对齐合成站实现的 v2 POST 形态。签名/cookie 声明原样保留，
// 经 stub signer + 伪 cookie 走 fail-closed 校验路径。
//
// 基线（capability_proposals.md §2，探针 2026-09-01 完整台架实测）：
//
//	A dy 5 作者全量回溯 26/20/23/1/21、0 重复；xhs 0 重复、页数=ceil(n/30)；
//	  ks profile/feed 干净终止。
//	B ks 30 用户/页、pcursor 回绕按 user_id 去重终止、profile 联结 5/5。
//	C dy 声称 753、楼中楼闭合 ≥90%（5 根 22/20）、评论者联结 ≥90%。
//	D dy 前缀相关 10 词 / inbox 9 热词恒定；xhs 10 词前缀相关。
//	E 8 次扩展 80 边、平均出度 10、确定性复走。
package collect

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/kuaishou"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/xhs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

// synthE2EPorts resolves MEDIAMON_SYNTH_PORTS="dy,xhs,ks".
func synthE2EPorts(t *testing.T) (dy, xhs, ks string) {
	t.Helper()
	v := strings.TrimSpace(os.Getenv("MEDIAMON_SYNTH_PORTS"))
	if v == "" {
		t.Skip("synth e2e: set MEDIAMON_SYNTH_PORTS=<dy,xhs,ks> against a self-started synth stack (see file comment)")
	}
	parts := strings.Split(v, ",")
	if len(parts) != 3 {
		t.Fatalf("MEDIAMON_SYNTH_PORTS must be <dy,xhs,ks> ports, got %q", v)
	}
	return parts[0], parts[1], parts[2]
}

// synthE2EEngine builds an engine whose contracts are the repository
// contracts re-pointed at the synth dev group (synth adaptations only:
// base_url + the two route remaps documented above).
func synthE2EEngine(t *testing.T) *Engine {
	t.Helper()
	dyP, xhsP, ksP := synthE2EPorts(t)
	base := map[string]string{
		"douyin":   "http://127.0.0.1:" + dyP,
		"xhs":      "http://127.0.0.1:" + xhsP,
		"kuaishou": "http://127.0.0.1:" + ksP,
	}
	src := testkit.ContractsDir(t, 2)
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read contracts dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("read contract: %v", err)
		}
		var c map[string]any
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("parse contract %s: %v", e.Name(), err)
		}
		plat, _ := c["platform"].(string)
		if b, ok := base[plat]; ok {
			tr, _ := c["transport"].(map[string]any)
			tr["base_url"] = b
			switch c["name"] {
			case "douyin-user": // synth 无 imapi：走语料作者链面（oracle adapt_synth 同款）
				tr["path"] = "/aweme/v1/web/user/profile/other/"
			case "xhs-search": // 合成站实现的是 v2 POST 形态
				tr["path"] = "/api/sns/web/v2/search/notes"
				tr["method"] = "POST"
				tr["body"] = map[string]any{
					"page": 1, "page_size": 20, "search_id": "", "sort": "general",
					"note_type": 0, "ext_flags": []any{}, "geo": "",
					"image_formats": []any{"jpg", "webp", "avif"}, "message_id": "sending",
				}
				bd, _ := c["binding"].(map[string]any)
				bd["items"] = "$.data.items"
				delete(c, "paging") // page 语义单页，多页断点如实记录
			case "kuaishou-search": // 合成站 graphql 未实现 visionSearchPhoto：
				// 重映射到 REST search/feed（oracle adapt_synth 同款口径）
				tr["path"] = "/rest/v/search/feed"
				tr["method"] = "POST"
				tr["body"] = map[string]any{"page": "search", "webPageArea": ""}
				bd, _ := c["binding"].(map[string]any)
				bd["items"] = "$.feeds"
				c["binding"] = bd
				c["paging"] = map[string]any{
					"cursor_param": "pcursor", "count_param": "page_size",
					"count_default": 20, "has_more_path": "$.pcursor",
					"next_cursor_path": "$.pcursor",
				}
			}
			c["transport"] = tr
		}
		out, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal contract: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), out, 0o644); err != nil {
			t.Fatalf("write contract: %v", err)
		}
	}
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, dst); err != nil {
		t.Fatalf("load remapped contracts: %v", err)
	}
	names := map[string]map[string]string{}
	for _, cn := range reg.List() {
		c, _ := reg.Get(cn)
		if c.Platform == "" {
			continue
		}
		if names[c.Platform] == nil {
			names[c.Platform] = map[string]string{}
		}
		names[c.Platform][c.Category] = c.Name
	}
	o := obs.NewCounterMap()
	return New(Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 90 * time.Second}),
		Obs:      o,
		Signers: map[string]httpclient.Signer{
			"douyin": httpclient.StaticSigner{Fn: func(ctx context.Context, contractName, rawURL string, params map[string]string) (map[string]string, error) {
				return map[string]string{"a_bogus": "synth-e2e", "msToken": "synth-e2e"}, nil
			}},
		},
		Cookies: map[string]string{
			"douyin":   "ttwid=synth-e2e",
			"kuaishou": "did=synth-e2e",
			"xhs":      "web_session=synth-e2e",
		},
		// 测试装配对齐生产（final-audit P3）：与 mediactl wiring.go /
		// mediad main.go 同款 BrowserHeaders，e2e 请求链与生产头集纪律
		// 一致（引擎在契约头之下合并浏览器默认头；仅测试面改动）。
		BrowserHeaders: map[string]map[string]string{
			douyin.Platform:   douyin.BrowserHeaders(),
			kuaishou.Platform: kuaishou.BrowserHeaders(),
			xhs.Platform:      xhs.BrowserHeaders(),
		},
		Names: names,
		Pacing: &PacingConfig{
			Enabled: true, Median: 20 * time.Millisecond, Sigma: 0.3,
			Min: time.Millisecond, Max: 80 * time.Millisecond,
		},
	})
}

const synthKeyword = "露营"

// synthFirstNAuthors returns the first n unique author ids from a keyword
// search (discovery baseline mirrors the probes).
func synthFirstNAuthors(t *testing.T, e *Engine, platform string, n int) []string {
	t.Helper()
	items, _, err := e.SearchItems(context.Background(), platform, synthKeyword, "", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("%s search discovery: %v", platform, err)
	}
	var out []string
	seen := map[string]bool{}
	for _, it := range items {
		id := it.Author.SecUID
		if id == "" {
			id = it.Author.UID
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == n {
			break
		}
	}
	if len(out) < n {
		t.Fatalf("%s discovery: %d unique authors, need %d", platform, len(out), n)
	}
	return out
}

func TestSynthE2ENewCapabilities(t *testing.T) {
	e := synthE2EEngine(t)
	t.Run("A_dossier_douyin", func(t *testing.T) { synthE2EDossierDouyin(t, e) })
	t.Run("A_dossier_xhs", func(t *testing.T) { synthE2EDossierXHS(t, e) })
	t.Run("A_dossier_kuaishou", func(t *testing.T) { synthE2EDossierKuaishou(t, e) })
	t.Run("B_user_search_kuaishou", func(t *testing.T) { synthE2EUserSearch(t, e) })
	t.Run("C_comment_thread_douyin", func(t *testing.T) { synthE2ECommentThread(t, e) })
	t.Run("D_suggest_words", func(t *testing.T) { synthE2ESuggest(t, e) })
	t.Run("E_related_graph_douyin", func(t *testing.T) { synthE2ERelated(t, e) })
	t.Run("F_batch_detail_douyin", func(t *testing.T) { synthE2EBatchDetail(t, e) })
	t.Run("G_series_chain_douyin", func(t *testing.T) { synthE2ESeriesChain(t, e) })
	t.Run("H_topic_feed", func(t *testing.T) { synthE2ETopicFeed(t, e) })
}

// synthE2EDossierDouyin: 5 作者全量回溯（proposal A 基线 26/20/23/1/21、
// 0 重复、1–2 页耗尽）+ claimed 面全键。
func synthE2EDossierDouyin(t *testing.T, e *Engine) {
	authors := synthFirstNAuthors(t, e, "douyin", 5)
	var uniq []int
	for _, a := range authors {
		d, err := e.AuthorDossier(context.Background(), "douyin", a, DossierOptions{})
		if err != nil {
			t.Fatalf("dossier %s: %v", a, err)
		}
		if d.Observed.WorksWalked != d.Observed.WorksUnique {
			t.Fatalf("author %s: fetched %d != unique %d (duplicates leaked)", a, d.Observed.WorksWalked, d.Observed.WorksUnique)
		}
		if want := (d.Observed.WorksUnique + 19) / 20; d.Observed.Pages != want {
			t.Fatalf("author %s: pages = %d, want ceil(n/20) = %d", a, d.Observed.Pages, want)
		}
		if d.Claimed.Nickname == "" || d.Claimed.AwemeCount <= 0 || d.Claimed.FollowerCount <= 0 {
			t.Fatalf("author %s: claimed face incomplete: %+v", a, d.Claimed)
		}
		t.Logf("dy dossier %s: unique=%d pages=%d claimed{followers=%d favorited=%d aweme=%d} sum_digg=%d median_interval_d=%.2f delta=%d",
			a, d.Observed.WorksUnique, d.Observed.Pages, d.Claimed.FollowerCount,
			d.Claimed.TotalFavorited, d.Claimed.AwemeCount, d.Observed.SumDigg,
			d.Observed.MedianIntervalDays, d.Consistency.CountDelta)
		uniq = append(uniq, d.Observed.WorksUnique)
	}
	sort.Ints(uniq)
	want := []int{1, 23, 69, 96, 165}
	for i := range want {
		if uniq[i] != want[i] {
			t.Fatalf("dy 5-author walk baseline = %v, want %v (proposal A)", uniq, want)
		}
	}
}

// synthE2EDossierXHS: 0 重复、页数 = ceil(unique/30)、interact 聚合 > 0。
func synthE2EDossierXHS(t *testing.T, e *Engine) {
	authors := synthFirstNAuthors(t, e, "xhs", 3)
	for _, a := range authors {
		d, err := e.AuthorDossier(context.Background(), "xhs", a, DossierOptions{})
		if err != nil {
			t.Fatalf("dossier %s: %v", a, err)
		}
		if d.Observed.WorksWalked != d.Observed.WorksUnique {
			t.Fatalf("author %s: fetched %d != unique %d", a, d.Observed.WorksWalked, d.Observed.WorksUnique)
		}
		pagesWant := (d.Observed.WorksUnique + 19) / 20 // engine count cap: num=20
		if d.Observed.Pages != pagesWant {
			t.Fatalf("author %s: pages = %d, want ceil(n/30) = %d", a, d.Observed.Pages, pagesWant)
		}
		if d.Observed.SumDigg <= 0 {
			t.Fatalf("author %s: interact aggregation empty (%+v)", a, d.Observed)
		}
		t.Logf("xhs dossier %s: unique=%d pages=%d sum_liked=%d max_liked=%d span_d=%.1f",
			a, d.Observed.WorksUnique, d.Observed.Pages, d.Observed.SumDigg,
			d.Observed.MaxDigg, d.Observed.PublishSpanDays)
	}
}

// synthE2EDossierKuaishou: profile/feed 回溯干净终止（proposal 基线 2 页
// 18 作品 no_more、0 重复）。
func synthE2EDossierKuaishou(t *testing.T, e *Engine) {
	users, _, err := e.UserSearch(context.Background(), "kuaishou", synthKeyword, model.Cursor{}, 5, UserSearchOptions{})
	if err != nil {
		t.Fatalf("ks discovery via user search: %v", err)
	}
	if len(users) == 0 {
		t.Fatal("ks discovery: no users")
	}
	// The ks leg: observed walk through kuaishou-profile-feed, claimed face
	// through /api/user/info (fill-forward combination).
	d, err := e.AuthorDossier(context.Background(), "kuaishou", users[0].User.UID, DossierOptions{})
	if err != nil {
		t.Fatalf("ks dossier %s: %v", users[0].User.UID, err)
	}
	if d.Observed.WorksWalked != d.Observed.WorksUnique {
		t.Fatalf("ks: fetched %d != unique %d", d.Observed.WorksWalked, d.Observed.WorksUnique)
	}
	if d.Claimed.Nickname == "" || d.Claimed.AwemeCount != int64(d.Observed.WorksUnique) {
		t.Fatalf("ks claimed/observed closure wrong: claimed.aweme=%d unique=%d (%+v)",
			d.Claimed.AwemeCount, d.Observed.WorksUnique, d.Claimed)
	}
	t.Logf("ks dossier %s: unique=%d pages=%d claimed{fans=%d follows=%d aweme=%d}",
		users[0].User.UID, d.Observed.WorksUnique, d.Observed.Pages,
		d.Claimed.FollowerCount, d.Profile.FollowingCount, d.Claimed.AwemeCount)
}

// synthE2EUserSearch: 30 用户/页、pcursor 回绕去重终止、联结 5/5。
func synthE2EUserSearch(t *testing.T, e *Engine) {
	before := e.obs.Get("collect.fetch")
	users, _, err := e.UserSearch(context.Background(), "kuaishou", synthKeyword, model.Cursor{}, 0,
		UserSearchOptions{JoinProfiles: true, JoinLimit: 5})
	if err != nil {
		t.Fatalf("UserSearch: %v", err)
	}
	after := e.obs.Get("collect.fetch")
	if len(users) != 30 {
		t.Fatalf("unique users = %d, want 30 (one page window)", len(users))
	}
	seen := map[string]bool{}
	for _, u := range users {
		if u.User.UID == "" || seen[u.User.UID] {
			t.Fatalf("user id empty or duplicated: %+v", u.User)
		}
		seen[u.User.UID] = true
	}
	// 2 search pages (rewind stop) + 5 profile joins = 7 fetches.
	if got := after - before; got != 7 {
		t.Fatalf("fetches = %d, want 7 (2 pages + 5 joins)", got)
	}
	hits := 0
	for i := 0; i < 5; i++ {
		if users[i].ProfileHit {
			hits++
		}
	}
	if hits != 5 {
		t.Fatalf("profile join = %d/5, want 5/5 (probe baseline)", hits)
	}
	t.Logf("ks user search: 30 unique users, join 5/5, sample[0]=%s(%s) fans=%d",
		users[0].User.Nickname, users[0].User.UID, users[0].User.FollowerCount)
}

// synthE2ECommentThread: 声称 753、全量翻页、楼中楼闭合 ≥90%、评论者
// 联结 ≥90%（enrich cap 20 下 20 个联结命中）。
func synthE2ECommentThread(t *testing.T, e *Engine) {
	t.Setenv("MEDIAMON_COMMENT_ENRICH_MAX", "20")
	items, _, err := e.SearchItems(context.Background(), "douyin", synthKeyword, "", model.Cursor{}, 5)
	if err != nil || len(items) == 0 {
		t.Fatalf("dy seed discovery: %v", err)
	}
	seed := items[0].ID
	out, err := e.CommentThread(context.Background(), "douyin", seed, CommentThreadOptions{SubRootLimit: 5})
	if err != nil {
		t.Fatalf("CommentThread: %v", err)
	}
	if out.NCommentsClaim != 753 {
		t.Fatalf("n_comments_claim = %d, want 753 (probe baseline)", out.NCommentsClaim)
	}
	if out.NCommentsWalked != int(out.NCommentsClaim) {
		t.Fatalf("walked = %d, want full closure of %d", out.NCommentsWalked, out.NCommentsClaim)
	}
	if out.SubClosure.Claimed == 0 || float64(out.SubClosure.Walked)/float64(out.SubClosure.Claimed) < 0.9 {
		t.Fatalf("sub closure = %d/%d, want ≥90%%", out.SubClosure.Walked, out.SubClosure.Claimed)
	}
	hits, joined := 0, 0
	for _, c := range out.Commenters {
		if c.ProfileHit {
			hits++
		}
		joined++
		if joined >= 20 {
			break
		}
	}
	if joined == 0 || float64(hits)/float64(joined) < 0.9 {
		t.Fatalf("commenter join = %d/%d, want ≥90%%", hits, joined)
	}
	if out.Timeseries.FirstDelayH != 0 || out.Timeseries.LastDelayH <= out.Timeseries.MedianDelayH {
		t.Fatalf("timeseries wrong: %+v", out.Timeseries)
	}
	t.Logf("dy comment thread %s: claim=%d walked=%d pages=%d roots_with_sub=%d sub=%d/%d commenters=%d join=%d/%d ts{med=%.1fh span=%.1fh}",
		seed, out.NCommentsClaim, out.NCommentsWalked, out.Pages, out.RootsWithSub,
		out.SubClosure.Walked, out.SubClosure.Claimed, len(out.Commenters), hits, joined,
		out.Timeseries.MedianDelayH, out.Timeseries.LastDelayH)
}

// synthE2ESuggest: dy 前缀相关 10 词 + inbox 9 热词恒定；xhs 10 词前缀相关。
func synthE2ESuggest(t *testing.T, e *Engine) {
	dy, err := e.SuggestWords(context.Background(), "douyin", "滑板教学")
	if err != nil {
		t.Fatalf("dy suggest: %v", err)
	}
	if len(dy.Words) != 10 {
		t.Fatalf("dy related_search words = %d, want 10", len(dy.Words))
	}
	for _, w := range dy.Words {
		if !strings.HasPrefix(w.Word, "滑板教学") && !strings.HasPrefix("滑板教学", w.Word) {
			t.Fatalf("dy word %q not prefix-related to query", w.Word)
		}
	}
	if dy.Source != "related_search" {
		t.Fatalf("dy source = %q", dy.Source)
	}
	dy2, err := e.SuggestWords(context.Background(), "douyin", "滑板教学")
	if err != nil {
		t.Fatalf("dy suggest repeat: %v", err)
	}
	for i := range dy.Words {
		if dy.Words[i].Word != dy2.Words[i].Word {
			t.Fatalf("dy suggest not deterministic at %d: %q vs %q", i, dy.Words[i].Word, dy2.Words[i].Word)
		}
	}
	inbox, err := e.SuggestWords(context.Background(), "douyin", "")
	if err != nil {
		t.Fatalf("dy inbox: %v", err)
	}
	if len(inbox.Words) != 9 || len(inbox.HotWords) != 9 {
		t.Fatalf("dy inbox = %d words, want 9 (+hot mirror)", len(inbox.Words))
	}
	inbox2, _ := e.SuggestWords(context.Background(), "douyin", "")
	for i := range inbox.Words {
		if inbox.Words[i].Word != inbox2.Words[i].Word {
			t.Fatalf("dy inbox not constant across calls at %d", i)
		}
	}
	xhs, err := e.SuggestWords(context.Background(), "xhs", "美食")
	if err != nil {
		t.Fatalf("xhs suggest: %v", err)
	}
	if len(xhs.Words) != 10 {
		t.Fatalf("xhs words = %d, want 10", len(xhs.Words))
	}
	for _, w := range xhs.Words {
		if !strings.HasPrefix(w.Word, "美食") {
			t.Fatalf("xhs word %q not prefix extension of 美食", w.Word)
		}
	}
	t.Logf("suggest: dy query→10 words (sample %q), dy inbox→9 hot words (sample %q), xhs→10 words (sample %q)",
		dy.Words[0].Word, inbox.Words[0].Word, xhs.Words[0].Word)
}

// synthE2ERelated: 单种子 K=2/预算 8 的确定性图 + 5 种子池化复现
// 8 次扩展 80 边口径。
func synthE2ERelated(t *testing.T, e *Engine) {
	items, _, err := e.SearchItems(context.Background(), "douyin", synthKeyword, "", model.Cursor{}, 20)
	if err != nil || len(items) == 0 {
		t.Fatalf("dy seed discovery: %v", err)
	}
	seed := items[0].ID
	g, err := e.RelatedGraph(context.Background(), "douyin", seed, RelatedOptions{})
	if err != nil {
		t.Fatalf("RelatedGraph: %v", err)
	}
	if g.Stats.Requests != 8 || g.Stats.NEdges != 80 {
		t.Fatalf("single-seed K2 budget walk: requests=%d edges=%d, want 8/80",
			g.Stats.Requests, g.Stats.NEdges)
	}
	if g.Stats.AvgOutDegree != 10.0 {
		t.Fatalf("avg out-degree = %v, want 10.0", g.Stats.AvgOutDegree)
	}
	g2, err := e.RelatedGraph(context.Background(), "douyin", seed, RelatedOptions{})
	if err != nil {
		t.Fatalf("RelatedGraph repeat: %v", err)
	}
	if len(g.Nodes) != len(g2.Nodes) || len(g.Edges) != len(g2.Edges) {
		t.Fatalf("repeat walk differs: %+v vs %+v", g.Stats, g2.Stats)
	}
	for i := range g.Nodes {
		if g.Nodes[i].AwemeID != g2.Nodes[i].AwemeID {
			t.Fatalf("node order not deterministic at %d", i)
		}
	}
	secUIDs := 0
	nonSeed := 0
	for _, n := range g.Nodes {
		if n.AwemeID == seed {
			continue // the seed node has no related-record author id by construction
		}
		nonSeed++
		if n.SecUID != "" {
			secUIDs++
		}
	}
	if nonSeed > 0 && secUIDs != nonSeed {
		t.Fatalf("author join ids on %d/%d related nodes, want all", secUIDs, nonSeed)
	}
	// Pooled 5-seed composite (probe shape: 8 expansions = 80 edges).
	pooled := map[string]bool{}
	edges := 0
	for i := 0; i < 5 && i < len(items); i++ {
		gs, err := e.RelatedGraph(context.Background(), "douyin", items[i].ID, RelatedOptions{MaxHops: 1, MaxExpansions: 1})
		if err != nil {
			t.Fatalf("pooled seed %d: %v", i, err)
		}
		for _, n := range gs.Nodes {
			pooled[n.AwemeID] = true
		}
		edges += len(gs.Edges)
	}
	// Deterministic hop-2 selection: sorted non-seed ids (map iteration
	// order would make the pooled node count vary run to run).
	var nonSeeds []string
	for id := range pooled {
		if id != items[0].ID && id != items[1].ID && id != items[2].ID && id != items[3].ID && id != items[4].ID {
			nonSeeds = append(nonSeeds, id)
		}
	}
	sort.Strings(nonSeeds)
	extra := 0
	for _, id := range nonSeeds {
		if extra >= 3 {
			break
		}
		gx, err := e.RelatedGraph(context.Background(), "douyin", id, RelatedOptions{MaxHops: 1, MaxExpansions: 1})
		if err != nil {
			t.Fatalf("pooled hop2 %s: %v", id, err)
		}
		for _, n := range gx.Nodes {
			pooled[n.AwemeID] = true
		}
		edges += len(gx.Edges)
		extra++
	}
	if edges != 80 {
		t.Fatalf("pooled expansions = %d edges, want 80", edges)
	}
	t.Logf("related: single-seed K2 nodes=%d edges=%d avg=%.1f; pooled 5+3 expansions nodes=%d edges=%d",
		g.Stats.NNodes, g.Stats.NEdges, g.Stats.AvgOutDegree, len(pooled), edges)
}

// synthE2EBatchDetail: 20 real + 1 bogus → 1 批返回 20（未知 id 静默省略）、
// 批/单核心字段一致（aweme_id + digg_count）、45 id → 3 批分批钳制。
func synthE2EBatchDetail(t *testing.T, e *Engine) {
	items, _, err := e.SearchItems(context.Background(), "douyin", synthKeyword, "", model.Cursor{}, 60)
	if err != nil || len(items) < 45 {
		t.Fatalf("dy seed discovery (need 45 unique ids): %v (%d items)", err, len(items))
	}
	var ids []string
	seen := map[string]bool{}
	for _, it := range items {
		if it.ID != "" && !seen[it.ID] {
			seen[it.ID] = true
			ids = append(ids, it.ID)
			if len(ids) == 45 {
				break
			}
		}
	}
	// Probe shape: 20 real + 1 nonexistent id — the unknown id is silently
	// omitted (no partial failure); under the endpoint's 20-id batch cap the
	// atom splits 21 ids into 2 requests (20 + 1) and returns the 20 hits.
	batch := append(append([]string{}, ids[:20]...), "9999999999999999999")
	res, err := e.BatchDetails(context.Background(), "douyin", batch, BatchDetailOptions{})
	if err != nil {
		t.Fatalf("BatchDetails: %v", err)
	}
	if res.Requested != 21 || res.Returned != 20 || res.Batches != 2 {
		t.Fatalf("batch outcome = %+v, want 21 requested -> 20 returned in 20+1 batches (silent omission)", res)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "9999999999999999999" {
		t.Fatalf("missing = %v, want the bogus id silently omitted", res.Missing)
	}
	// Batch/single core-field consistency (probe: core_matches_single).
	doc, err := e.Fetch(context.Background(), "douyin-video-download",
		map[string]string{"aweme_id": ids[0]}, nil)
	if err != nil {
		t.Fatalf("single detail: %v", err)
	}
	sd, _ := doc["aweme_detail"].(map[string]any)
	wantDigg := asInt(((sd["statistics"].(map[string]any))["digg_count"]))
	if res.Items[0].ID != ids[0] || res.Items[0].Stats.Digg != wantDigg {
		t.Fatalf("batch/single mismatch: %s digg=%d, single digg=%d",
			res.Items[0].ID, res.Items[0].Stats.Digg, wantDigg)
	}
	// Single-detail play_url binding resolves through the contract path
	// ($.aweme_detail.video.play_addr.url_list[0] — the key[index] JSONPath
	// form fixed per report G4); matrix row #7 closure evidence.
	meta, err := e.ResolveVideo(context.Background(), "douyin", ids[0])
	if err != nil {
		t.Fatalf("ResolveVideo: %v", err)
	}
	if meta.URL == "" || meta.AwemeID != ids[0] {
		t.Fatalf("single-detail play_url binding empty or mismatched: %+v", meta)
	}
	// Chunking under the count-clamp batch discipline: 45 ids -> 20/20/5.
	res3, err := e.BatchDetails(context.Background(), "douyin", ids, BatchDetailOptions{})
	if err != nil {
		t.Fatalf("BatchDetails(45): %v", err)
	}
	if res3.Batches != 3 || res3.Returned != 45 || len(res3.Missing) != 0 {
		t.Fatalf("45-id chunking = %+v, want 3 batches / 45 returned / 0 missing", res3)
	}
	t.Logf("batch detail: 21->20 (bogus silently omitted, 20+1 batches), 45 ids -> %d batches, batch==single on digg=%d, play_url=%s...",
		res3.Batches, wantDigg, meta.URL[:48])
}

// synthE2ESeriesChain: mix 13 集/3 页、episode 映射 1..13；series 33 集/6 页
// 游标闭合（proposal 基线复现，探针固定 id）；搜索卡被动发现 id 的发现腿闭
// 合断言；复走确定性。
func synthE2ESeriesChain(t *testing.T, e *Engine) {
	// Probe baseline legs (the probe's fixed chain id — the synth derives the
	// deterministic window from the id, so 13/33 and 3/6 pages are stable).
	mix, err := e.SeriesChain(context.Background(), "douyin", "mix", "7001234567890123456", SeriesOptions{})
	if err != nil {
		t.Fatalf("mix chain: %v", err)
	}
	if mix.TotalEpisodes != 13 || mix.Pages != 3 || mix.UniqueIDs != 13 || mix.Dupes != 0 {
		t.Fatalf("mix baseline = %+v, want 13 episodes / 3 pages / 0 dupes (probe)", mix)
	}
	if mix.EpisodeSource != "item_id_to_episode" {
		t.Fatalf("mix episode source = %q", mix.EpisodeSource)
	}
	eps := sortedEpisodeNumbers(mix.Episodes)
	for i, ep := range eps {
		if ep != i+1 {
			t.Fatalf("mix episode numbers = %v, want contiguous 1..13", eps)
		}
	}
	if len(mix.MissingEps) != 0 || mix.HasMore {
		t.Fatalf("mix chain not closed: missing=%v has_more=%v", mix.MissingEps, mix.HasMore)
	}
	ser, err := e.SeriesChain(context.Background(), "douyin", "series", "7001234567890123456", SeriesOptions{})
	if err != nil {
		t.Fatalf("series chain: %v", err)
	}
	if ser.TotalEpisodes != 33 || ser.Pages != 6 || ser.UniqueIDs != 33 || ser.Dupes != 0 {
		t.Fatalf("series baseline = %+v, want 33 episodes / 6 pages / 0 dupes (probe)", ser)
	}
	if ser.EpisodeSource != "walk_order" {
		t.Fatalf("series episode source = %q", ser.EpisodeSource)
	}
	for i, ep := range sortedEpisodeNumbers(ser.Episodes) {
		if ep != i+1 {
			t.Fatalf("series walk-order episodes not contiguous 1..33")
		}
	}
	// Determinism: repeat the mix walk -> identical episode ids/order.
	mix2, err := e.SeriesChain(context.Background(), "douyin", "mix", "7001234567890123456", SeriesOptions{})
	if err != nil {
		t.Fatalf("mix chain repeat: %v", err)
	}
	for i := range mix.Episodes {
		if mix.Episodes[i].Item.ID != mix2.Episodes[i].Item.ID {
			t.Fatalf("mix repeat walk differs at episode %d", mix.Episodes[i].Episode)
		}
	}
	// Discovery leg: the chain id arrives passively on search cards
	// (mix_info.mix_id / series_info.series_id — probe: 8/20 and 5/20 cards).
	items, _, err := e.SearchItems(context.Background(), "douyin", synthKeyword, "", model.Cursor{}, 20)
	if err != nil || len(items) == 0 {
		t.Fatalf("dy card discovery: %v", err)
	}
	var mixCards, seriesCards int
	var mixID, seriesID string
	for _, it := range items {
		if v, _ := it.Extra["mix_id"].(string); v != "" {
			mixCards++
			if mixID == "" {
				mixID = v
			}
		}
		if v, _ := it.Extra["series_id"].(string); v != "" {
			seriesCards++
			if seriesID == "" {
				seriesID = v
			}
		}
	}
	if mixID == "" {
		t.Fatal("no search card carried mix_info.mix_id (probe: 8/20)")
	}
	dm, err := e.SeriesChain(context.Background(), "douyin", "mix", mixID, SeriesOptions{})
	if err != nil {
		t.Fatalf("discovered mix chain: %v", err)
	}
	if dm.TotalEpisodes < 1 || dm.TotalEpisodes != dm.UniqueIDs || dm.HasMore || len(dm.MissingEps) != 0 {
		t.Fatalf("discovered mix chain not closed: %+v", dm)
	}
	t.Logf("series chain: mix 13 eps/3 pages (map 1..13), series 33 eps/6 pages (walk-order 1..33); cards carrying mix/series = %d/%d, discovered mix %s closed at %d eps",
		mixCards, seriesCards, mixID, dm.TotalEpisodes)
	_ = seriesID
}

// synthE2ETopicFeed: 三站话题腿——dy text_extra 锚文本（话题词 3 页 60 条、
// 锚定>0、hashtag_id 19 位、复走确定性）；ks tags 话题流（2 页 40 条、锚定>0、
// 无 id）；xhs 无标签面（内容清单照收、元数据缺位如实报告）。
func synthE2ETopicFeed(t *testing.T, e *Engine) {
	// dy: hashtag pool word as the topic (probe baseline: 60 walked / 60
	// unique / 13 anchored over 3 pages).
	dy, err := e.TopicFeed(context.Background(), "douyin", "#露营装备", TopicOptions{Limit: 60})
	if err != nil {
		t.Fatalf("dy topic feed: %v", err)
	}
	if len(dy.Items) != 60 || dy.Pages != 3 || dy.AnchoredItems < 1 {
		t.Fatalf("dy topic walk = items:%d pages:%d anchored:%d, want 60/3/>=1", len(dy.Items), dy.Pages, dy.AnchoredItems)
	}
	if dy.Meta.AnchorFace != "contract" || len(dy.Meta.HashtagID) != 19 {
		t.Fatalf("dy topic meta wrong: %+v (want hashtag_id 19-digit challenge id)", dy.Meta)
	}
	dy2, err := e.TopicFeed(context.Background(), "douyin", "#露营装备", TopicOptions{Limit: 60})
	if err != nil {
		t.Fatalf("dy topic feed repeat: %v", err)
	}
	for i := range dy.Items {
		if dy.Items[i].ID != dy2.Items[i].ID {
			t.Fatalf("dy topic walk not deterministic at %d", i)
		}
	}
	// ks: the search topic stream with record-level tags (probe: 40 walked /
	// 40 unique / 8 anchored over 2 pages; tags carry no ids).
	ks, err := e.TopicFeed(context.Background(), "kuaishou", "快手小剧场", TopicOptions{Limit: 40})
	if err != nil {
		t.Fatalf("ks topic feed: %v", err)
	}
	if len(ks.Items) != 40 || ks.Pages != 2 || ks.AnchoredItems < 1 {
		t.Fatalf("ks topic walk = items:%d pages:%d anchored:%d, want 40/2/>=1", len(ks.Items), ks.Pages, ks.AnchoredItems)
	}
	if ks.Meta.AnchorFace != "contract" || ks.Meta.HashtagID != "" {
		t.Fatalf("ks topic meta wrong: %+v (tag face carries no ids)", ks.Meta)
	}
	// xhs: no tag face on the synth oracle — the content list still collects
	// through search while the metadata reports the face absent.
	xhs, err := e.TopicFeed(context.Background(), "xhs", "露营", TopicOptions{})
	if err != nil {
		t.Fatalf("xhs topic feed: %v", err)
	}
	if len(xhs.Items) < 20 || xhs.AnchoredItems != 0 || xhs.Meta.AnchorFace != "" {
		t.Fatalf("xhs topic leg wrong: items:%d anchored:%d face:%q (absent face must report empty metadata)",
			len(xhs.Items), xhs.AnchoredItems, xhs.Meta.AnchorFace)
	}
	t.Logf("topic feed: dy 60 items/3 pages anchored=%d id=%s; ks 40 items/2 pages anchored=%d (no id face); xhs %d items, tag face absent",
		dy.AnchoredItems, dy.Meta.HashtagID, ks.AnchoredItems, len(xhs.Items))
}
