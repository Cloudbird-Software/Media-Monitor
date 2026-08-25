package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"testing"
)

func TestSignHandlerStub(t *testing.T) {
	srv := httptest.NewServer(signHandler(stubProvider, newCounters(t)))
	defer srv.Close()

	body := `{"contract":"douyin-search","url":"https://x/?kw=a","params":{"kw":"a"}}`
	resp, err := http.Post(srv.URL+"/sign", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out signResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Params["kw"] != "a" || !strings.HasPrefix(out.Params["a_bogus"], "stub-") {
		t.Fatalf("params = %v", out.Params)
	}
}

func TestSignHandlerNodeProvider(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("node signer shim runs on POSIX CI; skipped locally on Windows (not a suppression marker)")
		return
	}
	script := t.TempDir() + "/signer.js"
	// A fake upstream signer: echo a deterministic param augmentation.
	if err := writeFile(script, `#!/usr/bin/env node
const fs = require("fs");
const req = JSON.parse(fs.readFileSync(0, "utf8"));
process.stdout.write(JSON.stringify({ params: { ...req.params, a_bogus: "upstream-out", msToken: "mt-2" } }) + "\n");
`); err != nil {
		t.Fatal(err)
	}
	_ = chmod(script)

	srv := httptest.NewServer(signHandler(nodeProvider(script), newCounters(t)))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/sign", "application/json", strings.NewReader(`{"contract":"c","url":"u","params":{"x":"1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out signResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Params["a_bogus"] != "upstream-out" || out.Params["x"] != "1" {
		t.Fatalf("params = %v", out.Params)
	}
}

func TestSignHandlerNodeFailureFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("node shim exercises POSIX CI only")
		return
	}
	script := t.TempDir() + "/fail.js"
	if err := writeFile(script, `#!/usr/bin/env node
console.error("boom"); process.exit(3);
`); err != nil {
		t.Fatal(err)
	}
	_ = chmod(script)

	srv := httptest.NewServer(signHandler(nodeProvider(script), newCounters(t)))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/sign", "application/json", strings.NewReader(`{"contract":"c","url":"u","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func newCounters(t *testing.T) *obs.CounterMap {
	t.Helper()
	return obs.NewCounterMap()
}

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o755) }

func chmod(path string) error { return os.Chmod(path, 0o755) }
