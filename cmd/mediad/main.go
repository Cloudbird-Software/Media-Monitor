// Command mediad is the Media-Monitor daemon: REST health/tasks/metrics,
// a collect API (contract-driven, same parameters as the MCP tools), and a
// minimal status dashboard with task statistics, offline canary summary and
// metrics text. The collect engine is wired from the adapt dir
// (MEDIAMON_ADAPT_DIR, default "adapt"); a missing or broken adapt tree
// degrades the daemon (collect endpoints return 503) instead of preventing
// it from starting.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adapt"
	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/core"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/kuaishou"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/xhs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/signclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
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

	d := &daemon{runner: runner, counters: counters}
	d.wireAdapt()
	if d.adaptErr != nil {
		log.Printf("warn: %v (collect API degraded, canary summary unavailable)", d.adaptErr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obs.HealthNow(true))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, counters.MetricsText())
	})
	mux.HandleFunc("/api/v1/tasks", taskHandler(runner))
	mux.HandleFunc("/api/v1/collect/", d.collectHandler)
	mux.HandleFunc("/", d.dashboardHandler)

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("mediad listening on %s (store %s)", *addr, *dir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
	sh, cancelSh := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSh()
	_ = srv.Shutdown(sh)
	log.Println("mediad stopped")
}

// daemon carries the daemon-wide dependencies between handlers.
type daemon struct {
	runner   *core.Runner
	counters *obs.CounterMap
	reg      *contracts.Registry // nil when the adapt tree is unavailable
	engine   *collect.Engine     // nil when the adapt tree is unavailable
	adaptErr error
	canary   *canaryStatus // computed once at startup
}

// canaryStatus is the startup-time offline canary snapshot shown on the
// dashboard.
type canaryStatus struct {
	Summary string
	Healthy bool
	Cases   int
	Err     string
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
// from the adapt tree. Failures are recorded on the daemon (non-fatal).
func (d *daemon) wireAdapt() {
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
	d.engine = collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{}),
		Obs:      d.counters,
		Signers:  signers,
		Cookies:  cookies,
		Names:    names,
	})
	d.reg = reg
	d.canary = runCanaries(reg, dir)
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

// collectHandler serves POST /api/v1/collect/{search|comments|replies|user|group}.
// The JSON body uses the same parameter names as the MCP tools.
func (d *daemon) collectHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	op := strings.TrimPrefix(req.URL.Path, "/api/v1/collect/")
	switch op {
	case "search", "comments", "replies", "user", "group":
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
	switch op {
	case "search":
		keyword := strVal(body, "keyword")
		if keyword == "" {
			return nil, errors.New("keyword is required")
		}
		items, cur, err := d.engine.SearchItems(ctx, platform, keyword, strVal(body, "media_type"), model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": items, "cursor": cur}, nil
	case "comments":
		itemID := strVal(body, "item_id")
		if itemID == "" {
			return nil, errors.New("item_id is required")
		}
		cmts, cur, err := d.engine.ItemComments(ctx, platform, itemID, model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"comments": cmts, "cursor": cur}, nil
	case "replies":
		itemID := strVal(body, "item_id")
		cid := strVal(body, "cid")
		if itemID == "" || cid == "" {
			return nil, errors.New("item_id and cid are required")
		}
		cmts, cur, err := d.engine.CommentReplies(ctx, platform, itemID, cid, model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"comments": cmts, "cursor": cur}, nil
	case "user":
		secUID := strVal(body, "sec_uid")
		if secUID == "" {
			return nil, errors.New("sec_uid is required")
		}
		u, err := d.engine.UserProfile(ctx, platform, secUID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"user": u}, nil
	case "group":
		groupID := strVal(body, "group_id")
		if groupID == "" {
			return nil, errors.New("group_id is required")
		}
		members, cur, err := d.engine.GroupMembers(ctx, platform, groupID, model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"members": members, "cursor": cur}, nil
	}
	return nil, fmt.Errorf("unknown collect op %q", op)
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

func taskHandler(r *core.Runner) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			tasks, err := r.List()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, 200, tasks)
		case http.MethodPost:
			var body struct {
				Kind   string        `json:"kind"`
				Config model.JSONMap `json:"config"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
				return
			}
			task, err := r.Submit(body.Kind, body.Config)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, 201, task)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
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
