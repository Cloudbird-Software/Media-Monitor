// Command mediad is the Media-Monitor daemon: REST health/tasks/metrics,
// a collect API (contract-driven, same parameters as the MCP tools), and a
// minimal status dashboard with task statistics, offline canary summary and
// metrics text. The collect engine is wired from the adapt dir
// (MEDIAMON_ADAPT_DIR, default "adapt"); a missing or broken adapt tree
// degrades the daemon (collect endpoints return 503) instead of preventing
// it from starting.
//
// Startup wiring (see wiring.go): the account pool (MEDIAMON_ACCOUNTS_DIR,
// default <data>/accounts) is injected into the collect engine so any
// collect/send request can act as a pool account via "account_id"; the UA
// pool (MEDIAMON_UA_POOL, default data/ua-pool.json next to the executable)
// feeds the shared HTTP client (missing file = keep built-in pool, never
// fatal); the license gate (MEDIAMON_LICENSE_DIR / MEDIAMON_LICENSE_PUBKEY,
// MEDIAMON_LICENSE_REQUIRED=false to disable) refuses collect/send/task
// execution without a valid license; the datacenter hub
// (MEDIAMON_DATACENTER_DIR, webhook via MEDIAMON_WEBHOOK_URL /
// MEDIAMON_WEBHOOK_MIN_INTERVAL / MEDIAMON_WEBHOOK_MAX_INTERVAL) aggregates
// every successful collect/send output.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/adapt"
	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/core"
	"github.com/Cloudbird-Software/Media-Monitor/internal/datacenter"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/license"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/kuaishou"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/xhs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/signclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
	"github.com/Cloudbird-Software/Media-Monitor/internal/tasks"
)

func main() {
	dir := flag.String("dir", "data", "store directory")
	addr := flag.String("addr", "127.0.0.1:8088", "listen address")
	flag.Parse()

	if env := os.Getenv("MEDIAMON_DATA_DIR"); env != "" {
		*dir = env
	}
	st, err := store.Open(*dir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	counters := obs.NewCounterMap()
	runner := core.NewRunner(st, counters)

	d := &daemon{runner: runner, counters: counters, store: st, im: newIMPoller()}
	d.wireAdapt(*dir)
	if d.adaptErr != nil {
		log.Printf("warn: %v (collect API degraded, canary summary unavailable)", d.adaptErr)
	}
	d.wireLicense(*dir)
	log.Printf("license gate: %s", d.licenseNote)
	d.wireDatacenter(*dir)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	d.ctx = ctx
	go d.startPushLoop(ctx)

	srv := &http.Server{Addr: *addr, Handler: d.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("mediad listening on %s (store %s)", *addr, *dir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	sh, cancelSh := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSh()
	_ = srv.Shutdown(sh)
	d.finalFlush(sh) // webhook final flush + hub store close
	d.Close()
	log.Println("mediad stopped")
}

// daemon carries the daemon-wide dependencies between handlers.
type daemon struct {
	runner     *core.Runner
	counters   *obs.CounterMap
	reg        *contracts.Registry // nil when the adapt tree is unavailable
	engine     *collect.Engine     // nil when the adapt tree is unavailable
	collectCtx collect.Context     // base engine wiring (account_id clones it)
	adaptErr   error
	canary     *canaryStatus // computed once at startup
	accounts   *accounts.Pool
	store      *store.Store
	// license gate (nil = disabled via MEDIAMON_LICENSE_REQUIRED=false).
	gate        *license.Gate
	licenseNote string
	// datacenter hub + webhook push state.
	hub          *datacenter.Hub
	webhookDesc  string
	pushInterval time.Duration // test-injectable; default defaultPushInterval
	dcIngest     atomic.Int64
	dcAdded      atomic.Int64
	// IM unread polling state for the dashboard.
	im *imPoller
	// ctx is the daemon lifetime context (drives background pollers).
	ctx context.Context
}

// canaryStatus is the startup-time offline canary snapshot shown on the
// dashboard.
type canaryStatus struct {
	Summary string
	Healthy bool
	Cases   int
	Err     string
}

// Close releases the account pool's underlying store.
func (d *daemon) Close() {
	if d.accounts != nil {
		_ = d.accounts.Close()
	}
}

// routes exposes the handler tree (used by main and the tests).
func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obs.HealthNow(true))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, d.counters.MetricsText())
	})
	mux.HandleFunc("/api/v1/tasks", d.tasksHandler)
	mux.HandleFunc("/api/v1/collect/", d.collectHandler)
	mux.HandleFunc("/api/v1/send", d.sendHandler)
	mux.HandleFunc("/api/v1/accounts", d.accountsHandler)
	mux.HandleFunc("/", d.dashboardHandler)
	return mux
}

// wireLicense loads the license gate (never fatal; see loadLicenseGate).
func (d *daemon) wireLicense(dataDir string) {
	d.gate, d.licenseNote = loadLicenseGate(dataDir)
}

// adaptDirEnv resolves the adapt tree like the other commands
// (MEDIAMON_ADAPT_DIR override, default "adapt" relative to CWD).
func adaptDirEnv() string {
	if d := os.Getenv("MEDIAMON_ADAPT_DIR"); d != "" {
		return d
	}
	return "adapt"
}

// wireAdapt assembles the collect engine and the offline canary snapshot
// from the adapt tree. Failures are recorded on the daemon (non-fatal). The
// account pool is opened first so the engine can route per-account requests.
func (d *daemon) wireAdapt(dataDir string) {
	if p, err := accounts.Open(accountsDirEnv(dataDir)); err == nil {
		d.accounts = p
	} else {
		log.Printf("warn: account pool unavailable: %v (account_id routing disabled)", err)
	}
	dir := adaptDirEnv()
	cdir := filepath.Join(dir, "contracts")
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, cdir); err != nil {
		d.adaptErr = fmt.Errorf("contracts dir %s: %w", cdir, err)
		d.canary = &canaryStatus{Err: d.adaptErr.Error()}
		return
	}
	names := map[string]map[string]string{}
	dou, _, err := douyin.Defaults(cdir)
	if err != nil {
		d.adaptErr = err
		d.canary = &canaryStatus{Err: err.Error()}
		return
	}
	ks, _, err := kuaishou.Defaults(cdir)
	if err != nil {
		d.adaptErr = err
		d.canary = &canaryStatus{Err: err.Error()}
		return
	}
	xh, _, err := xhs.Defaults(cdir)
	if err != nil {
		d.adaptErr = err
		d.canary = &canaryStatus{Err: err.Error()}
		return
	}
	names[douyin.Platform] = dou.Names
	names[kuaishou.Platform] = ks.Names
	names[xhs.Platform] = xh.Names

	signers := map[string]httpclient.Signer{}
	if u := os.Getenv("MEDIAMON_SIGNER_URL"); u != "" {
		sc := signclient.New(signclient.Config{BaseURL: u}) // fail-closed
		for _, p := range []string{douyin.Platform, kuaishou.Platform, xhs.Platform} {
			signers[p] = sc
		}
	}
	cookies := map[string]string{}
	if v := os.Getenv("MEDIAMON_DOUYIN_COOKIES"); v != "" {
		cookies[douyin.Platform] = v
	}
	if v := os.Getenv("MEDIAMON_KUAISHOU_COOKIES"); v != "" {
		cookies[kuaishou.Platform] = v
	}
	if v := os.Getenv("MEDIAMON_XHS_COOKIES"); v != "" {
		cookies[xhs.Platform] = v
	}
	d.collectCtx = collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{UserAgents: uaPoolUserAgents()}),
		Obs:      d.counters,
		Signers:  signers,
		Cookies:  cookies,
		Names:    names,
		Accounts: d.accounts,
	}
	d.engine = collect.New(d.collectCtx)
	d.reg = reg
	d.canary = runCanaries(reg, dir)
}

// engineFor returns the engine to serve one request: the shared engine for
// platform defaults, or a per-request clone pinned to a pool account (the
// account's cookie/proxy/UA then override the platform defaults).
func (d *daemon) engineFor(accountID string) *collect.Engine {
	if d.engine == nil {
		return nil
	}
	if accountID == "" || d.accounts == nil {
		return d.engine
	}
	ctx := d.collectCtx
	ctx.AccountID = accountID
	return collect.New(ctx)
}

// runCanaries executes the offline canary suite once (startup time).
func runCanaries(reg *contracts.Registry, dir string) *canaryStatus {
	runner := adapt.NewRunner(reg, filepath.Join(dir, "fixtures"), filepath.Join(dir, "canaries"))
	reports, err := runner.RunAllOffline()
	if err != nil {
		return &canaryStatus{Err: err.Error()}
	}
	st := &canaryStatus{Summary: contracts.Summarize(reports), Cases: len(reports), Healthy: true}
	for _, r := range reports {
		if !r.Healthy() {
			st.Healthy = false
		}
	}
	return st
}

// ---- collect API ----

// collectHandler serves POST /api/v1/collect/{search|comments|replies|user|
// group|video|collects|collects-videos|im-unread}. The JSON body uses the
// same parameter names as the MCP tools; an optional "account_id" routes the
// request through that pool account's cookie/proxy/UA. Gated by license.
func (d *daemon) collectHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !d.checkGate(w) {
		return
	}
	op := strings.TrimPrefix(req.URL.Path, "/api/v1/collect/")
	switch op {
	case "search", "comments", "replies", "user", "group", "video", "collects", "collects-videos", "im-unread":
	default:
		http.NotFound(w, req)
		return
	}
	if d.engine == nil {
		msg := "collect engine unavailable"
		if d.adaptErr != nil {
			msg += ": " + d.adaptErr.Error()
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": msg})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	platform := strVal(body, "platform")
	if !d.validPlatform(platform) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("platform %q is not a known platform (want douyin|kuaishou|xhs or a platform with registered contracts)", platform)})
		return
	}
	result, err := d.runCollect(op, req.Context(), body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (d *daemon) runCollect(op string, ctx context.Context, body map[string]any) (any, error) {
	platform := strVal(body, "platform")
	limit := intVal(body, "limit", 20)
	eng := d.engineFor(strVal(body, "account_id"))
	if eng == nil {
		return nil, errors.New("collect engine unavailable")
	}
	switch op {
	case "search":
		keyword := strVal(body, "keyword")
		if keyword == "" {
			return nil, errors.New("keyword is required")
		}
		items, cur, err := eng.SearchItems(ctx, platform, keyword, strVal(body, "media_type"), model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		d.hubAdd(itemRecords(platform, items)...)
		return map[string]any{"items": items, "cursor": cur}, nil
	case "comments":
		itemID := strVal(body, "item_id")
		if itemID == "" {
			return nil, errors.New("item_id is required")
		}
		cmts, cur, err := eng.ItemComments(ctx, platform, itemID, model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		d.hubAdd(commentRecords(platform, cmts)...)
		return map[string]any{"comments": cmts, "cursor": cur}, nil
	case "replies":
		itemID := strVal(body, "item_id")
		cid := strVal(body, "cid")
		if itemID == "" || cid == "" {
			return nil, errors.New("item_id and cid are required")
		}
		cmts, cur, err := eng.CommentReplies(ctx, platform, itemID, cid, model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		d.hubAdd(commentRecords(platform, cmts)...)
		return map[string]any{"comments": cmts, "cursor": cur}, nil
	case "user":
		secUID := strVal(body, "sec_uid")
		if secUID == "" {
			return nil, errors.New("sec_uid is required")
		}
		u, err := eng.UserProfile(ctx, platform, secUID)
		if err != nil {
			return nil, err
		}
		d.hubAdd(profileRecord(platform, u, "user")...)
		return map[string]any{"user": u}, nil
	case "group":
		groupID := strVal(body, "group_id")
		if groupID == "" {
			return nil, errors.New("group_id is required")
		}
		members, cur, err := eng.GroupMembers(ctx, platform, groupID, model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		d.hubAdd(memberRecords(platform, members)...)
		return map[string]any{"members": members, "cursor": cur}, nil
	case "video":
		itemID := strVal(body, "item_id")
		if itemID == "" {
			return nil, errors.New("item_id is required")
		}
		meta, err := eng.ResolveVideo(ctx, platform, itemID)
		if err != nil {
			return nil, err
		}
		d.hubAdd(datacenter.Record{
			Platform: platform,
			UserKey:  meta.AwemeID,
			Payload:  map[string]any{"kind": "video", "url": meta.URL, "cover": meta.Cover},
		})
		// The watermark-free address is returned; downloading the bytes is
		// left to mediactl / the caller.
		return map[string]any{"video": meta}, nil
	case "collects":
		folders, cur, err := eng.CollectFolders(ctx, platform, model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		d.hubAdd(itemRecords(platform, folders)...)
		return map[string]any{"collects": folders, "cursor": cur}, nil
	case "collects-videos":
		collectsID := strVal(body, "collects_id")
		if collectsID == "" {
			return nil, errors.New("collects_id is required")
		}
		items, cur, err := eng.CollectVideos(ctx, platform, collectsID, model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		d.hubAdd(itemRecords(platform, items)...)
		return map[string]any{"items": items, "cursor": cur}, nil
	case "im-unread":
		res, err := eng.FetchIMUnread(ctx, platform)
		if err != nil {
			return nil, err
		}
		accountID := strVal(body, "account_id")
		key := accountID
		if key == "" {
			key = "default"
		}
		d.hubAdd(datacenter.Record{
			Platform: platform,
			UserKey:  key,
			Payload:  map[string]any{"kind": "im_unread", "total_unread": res.TotalUnread, "conversations": len(res.Conversations)},
		})
		return map[string]any{"im_unread": res}, nil
	}
	return nil, fmt.Errorf("unknown collect op %q", op)
}

// ---- send API ----

// sendHandler serves POST /api/v1/send: run a direct-message broadcast job.
// Gated by license; cfg.AccountId ("account_id") routes through the pool.
func (d *daemon) sendHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !d.checkGate(w) {
		return
	}
	if d.engine == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "collect engine unavailable"})
		return
	}
	var cfg tasks.SendTaskConfig
	if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	rep, err := tasks.NewSender(d.engineFor(cfg.AccountID), d.store).Run(req.Context(), cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var recs []datacenter.Record
	for _, oc := range rep.Results {
		if oc.Target == "" || oc.Error != "" {
			continue
		}
		recs = append(recs, datacenter.Record{
			Platform: cfg.Platform,
			UserKey:  oc.Target,
			Payload:  map[string]any{"kind": "send", "first_status": oc.FirstStatus, "second_status": oc.SecondStatus},
		})
	}
	d.hubAdd(recs...)
	writeJSON(w, http.StatusOK, rep)
}

// ---- accounts API ----

// accountsHandler serves GET/POST /api/v1/accounts: list (GET) or import
// (POST) accounts. Export/delete are exposed via the CLI and MCP; the REST
// surface covers the read/add path used by dashboards.
func (d *daemon) accountsHandler(w http.ResponseWriter, req *http.Request) {
	if d.accounts == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "account pool unavailable"})
		return
	}
	switch req.Method {
	case http.MethodGet:
		platform := req.URL.Query().Get("platform")
		out := []accounts.Account{}
		for _, acct := range d.accounts.List() {
			if platform != "" && acct.Platform != platform {
				continue
			}
			out = append(out, acct)
		}
		writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// validPlatform accepts the three canonical platforms plus any platform that
// has contracts registered (self-made/test platforms and future additions).
func (d *daemon) validPlatform(p string) bool {
	if p == douyin.Platform || p == kuaishou.Platform || p == xhs.Platform {
		return true
	}
	return d.reg != nil && len(d.reg.Platform(p)) > 0
}

// ---- dashboard ----

func (d *daemon) dashboardHandler(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>mediad</title>
<style>body{font-family:system-ui,sans-serif;margin:2rem;max-width:60rem}pre{background:#f6f8fa;padding:1rem;overflow:auto}table{border-collapse:collapse}td,th{border:1px solid #ccc;padding:.3rem .8rem;text-align:left}</style></head><body>
<h1>media-monitor daemon</h1>
<ul><li><a href="/healthz">health</a></li><li><a href="/metrics">metrics</a></li>
<li><a href="/api/v1/tasks">tasks</a></li><li><a href="/api/v1/collect/search">collect API</a></li></ul>
`)
	stats := d.taskStats()
	fmt.Fprint(w, `<h2>tasks</h2><table><tr><th>state</th><th>count</th></tr>`)
	for _, k := range []string{"queued", "running", "done", "failed", "cancelled"} {
		fmt.Fprintf(w, "<tr><td>%s</td><td>%d</td></tr>\n", k, stats[k])
	}
	fmt.Fprintf(w, `<tr><td>total</td><td>%d</td></tr></table>`, stats["total"])
	fmt.Fprint(w, `<h2>offline canary summary (at startup)</h2><pre>`)
	switch {
	case d.canary == nil:
		fmt.Fprint(w, "not run</pre>")
	case d.canary.Err != "":
		fmt.Fprintf(w, "%s</pre>", html.EscapeString("error: "+d.canary.Err))
	default:
		fmt.Fprintf(w, "%shealthy: %t (%d cases)</pre>", html.EscapeString(d.canary.Summary), d.canary.Healthy, d.canary.Cases)
	}
	fmt.Fprint(w, `<h2>datacenter</h2>`)
	if d.hub == nil {
		fmt.Fprint(w, `<p>hub unavailable</p>`)
	} else {
		stats := d.datacenterStats()
		fmt.Fprint(w, `<table><tr><th>metric</th><th>value</th></tr>`)
		fmt.Fprintf(w, "<tr><td>ingested (pre-dedup)</td><td>%d</td></tr>\n", stats.Ingested)
		fmt.Fprintf(w, "<tr><td>added (post-dedup)</td><td>%d</td></tr>\n", stats.Added)
		fmt.Fprintf(w, "<tr><td>stored</td><td>%d</td></tr>\n", stats.Stored)
		fmt.Fprintf(w, "<tr><td>webhook</td><td>%s</td></tr></table>\n", html.EscapeString(d.webhookDesc))
		recs := d.hub.List(nil, false)
		if len(recs) > 5 {
			recs = recs[:5]
		}
		fmt.Fprint(w, `<table><tr><th>platform</th><th>user_key</th><th>nickname</th><th>time</th></tr>`)
		for _, r := range recs {
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
				html.EscapeString(r.Platform), html.EscapeString(r.UserKey),
				html.EscapeString(r.Nickname), html.EscapeString(time.Unix(r.Timestamp, 0).Format(time.RFC3339)))
		}
		fmt.Fprint(w, `</table>`)
	}
	fmt.Fprint(w, `<h2>IM unread</h2>`)
	if d.im == nil || len(d.im.snapshot()) == 0 {
		fmt.Fprint(w, `<p>no polls yet (submit a task with kind "im-unread-poll")</p>`)
	} else {
		fmt.Fprint(w, `<table><tr><th>platform</th><th>account</th><th>unread</th><th>conversations</th><th>last poll</th><th>error</th></tr>`)
		for _, s := range d.im.snapshot() {
			acct := s.AccountID
			if acct == "" {
				acct = "(default)"
			}
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%d</td><td>%d</td><td>%s</td><td>%s</td></tr>\n",
				html.EscapeString(s.Platform), html.EscapeString(acct), s.TotalUnread, s.Conversations,
				html.EscapeString(time.Unix(s.LastPoll, 0).Format(time.RFC3339)), html.EscapeString(s.Error))
		}
		fmt.Fprint(w, `</table>`)
	}
	fmt.Fprint(w, `<h2>metrics</h2><pre>`)
	fmt.Fprint(w, html.EscapeString(d.counters.MetricsText()))
	fmt.Fprint(w, `</pre></body></html>`)
}

// taskStats counts tasks per state.
func (d *daemon) taskStats() map[string]int64 {
	stats := map[string]int64{"total": 0}
	tasks, err := d.runner.List()
	if err != nil {
		return stats
	}
	for _, t := range tasks {
		stats[t.State]++
		stats["total"]++
	}
	return stats
}

// ---- shared handlers and helpers ----

// tasksHandler serves GET/POST /api/v1/tasks. Listing is license-exempt;
// submitting is gated. A submitted "im-unread-poll" task additionally starts
// its background polling loop (config: platform, account_id,
// interval_seconds).
func (d *daemon) tasksHandler(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		tasks, err := d.runner.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, 200, tasks)
	case http.MethodPost:
		if !d.checkGate(w) {
			return
		}
		var body struct {
			Kind   string        `json:"kind"`
			Config model.JSONMap `json:"config"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		task, err := d.runner.Submit(body.Kind, body.Config)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if body.Kind == "im-unread-poll" {
			d.startIMPoll(task)
		}
		writeJSON(w, 201, task)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// strVal / intVal coerce JSON values decoded into map[string]any.
func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func intVal(m map[string]any, key string, def int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}
