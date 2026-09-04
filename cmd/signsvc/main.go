// Command signsvc is the signature service: an HTTP endpoint speaking the
// signclient protocol (POST /sign {contract,url,params} -> {params}) whose
// implementations are pluggable. Production deployments point the node
// provider at an upstream open-source signer script (deployment-owned; see
// upstream/registry.json and docs/HARDENING.md M3 — the algorithm never
// ships inside the client artifact). Dev/staging can use the stub provider,
// which is explicitly unfit for production traffic.
package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
)

type signRequest struct {
	Contract string            `json:"contract"`
	URL      string            `json:"url"`
	Params   map[string]string `json:"params"`
}

type signResponse struct {
	Params map[string]string `json:"params"`
	Error  string            `json:"error,omitempty"`
}

// Provider computes the signed parameter augmentation for one request.
type Provider func(ctx context.Context, req signRequest) (map[string]string, error)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	mode := flag.String("provider", "node", "provider: node|stub")
	script := flag.String("node-js", "", "path to the node signer script (provider=node required)")
	flag.Parse()

	var p Provider
	switch *mode {
	case "stub":
		p = stubProvider
	case "node":
		if *script == "" {
			log.Fatal("--provider node requires --node-js <script path>")
		}
		p = nodeProvider(*script)
	default:
		log.Fatalf("unknown provider %q", *mode)
	}

	counters := obs.NewCounterMap()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, counters.MetricsText())
	})
	mux.HandleFunc("/sign", signHandler(p, counters))

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("signsvc listening on %s (provider=%s)", *addr, *mode)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	_ = srv.Shutdown(shCtx)
	log.Println("signsvc stopped")
}

func signHandler(p Provider, counters *obs.CounterMap) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req signRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		counters.Inc("sign.requests", 1)
		out, err := p(r.Context(), req)
		if err != nil {
			counters.Inc("sign.errors", 1)
			writeJSON(w, http.StatusServiceUnavailable, signResponse{Error: err.Error()})
			return
		}
		counters.Inc("sign.success", 1)
		writeJSON(w, http.StatusOK, signResponse{Params: out})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// nodeProvider delegates to an upstream node signer script:
//
//	stdin : the signRequest JSON
//	argv  : sign <contract> <url>
//	stdout: {"params": {...}}
//
// A non-zero exit or unparsable stdout fails closed.
func nodeProvider(script string) Provider {
	return func(ctx context.Context, req signRequest) (map[string]string, error) {
		payload, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("signsvc: marshal: %w", err)
		}
		cmd := exec.CommandContext(ctx, "node", script, "sign", req.Contract, req.URL)
		cmd.Stdin = bytes.NewReader(payload)
		var out bytes.Buffer
		var errBuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("signsvc: node script: %w (%s)", err, strings.TrimSpace(errBuf.String()))
		}
		var resp signResponse
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			return nil, fmt.Errorf("signsvc: node script stdout: %w", err)
		}
		if len(resp.Params) == 0 {
			return nil, fmt.Errorf("signsvc: node script returned empty params")
		}
		return resp.Params, nil
	}
}

// stubHeaderDecls maps contract name -> the header-carried signature values
// its contract declares (signature.headers, e.g. xhs x-s / x-s-common).
// Loaded once from the adapt contracts dir so the stub can also feed
// header-signature contracts in dev (report item 10 / FC7: the old stub only
// produced a_bogus, so xhs-style contracts fail-closed even in dev).
var stubHeaderDecls = loadStubHeaderDecls()

func loadStubHeaderDecls() map[string][]string {
	dir := os.Getenv("MEDIAMON_ADAPT_DIR")
	if dir == "" {
		dir = "adapt"
	}
	out := map[string][]string{}
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, filepath.Join(dir, "contracts")); err != nil {
		return out
	}
	for _, name := range reg.List() {
		if c, ok := reg.Get(name); ok && len(c.Signature.Headers) > 0 {
			out[name] = append([]string(nil), c.Signature.Headers...)
		}
	}
	return out
}

// stubProvider is dev-only: it appends a deterministic marker parameter and
// must never reach production (docs/HARDENING.md M3). It also emits stub
// values for every header the contract routes through signature.headers so
// dev environments can exercise xhs-style header-signed contracts.
func stubProvider(_ context.Context, req signRequest) (map[string]string, error) {
	out := make(map[string]string, len(req.Params)+1)
	for k, v := range req.Params {
		out[k] = v
	}
	sum := md5.Sum([]byte(req.Contract + "|" + req.URL))
	out["a_bogus"] = "stub-" + hex.EncodeToString(sum[:6])
	for _, h := range stubHeaderDecls[req.Contract] {
		if out[h] == "" {
			out[h] = "stub-" + hex.EncodeToString(sum[:8])
		}
	}
	return out, nil
}
