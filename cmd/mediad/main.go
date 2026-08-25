// Command mediad is the Media-Monitor daemon: REST health/tasks, metrics
// endpoint, and a minimal status dashboard. Collection/live/mobile
// endpoints mount on the same mux as later PRs wire the engines.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/core"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

func main() {
	dir := flag.String("dir", "data", "store directory")
	addr := flag.String("addr", "127.0.0.1:8088", "listen address")
	flag.Parse()

	st, err := store.Open(*dir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	counters := obs.NewCounterMap()
	runner := core.NewRunner(st, counters)

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
	mux.HandleFunc("/", dashboardHandler)

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

func dashboardHandler(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html><head><title>mediad</title></head><body>
<h1>media-monitor daemon</h1><ul><li><a href="/healthz">health</a></li>
<li><a href="/metrics">metrics</a></li><li><a href="/api/v1/tasks">tasks</a></li></ul>
</body></html>`)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
