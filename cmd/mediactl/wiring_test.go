package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
)

func TestSharedHTTPClientUAPoolInjected(t *testing.T) {
	dir := t.TempDir()
	poolFile := filepath.Join(dir, "ua-pool.json")
	if err := os.WriteFile(poolFile, []byte(`{"uas":["UA-A","UA-B","UA-C"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEDIAMON_UA_POOL", poolFile)

	uas := uaPoolUserAgents()
	if len(uas) != 3 {
		t.Fatalf("uaPoolUserAgents = %v, want 3 entries", uas)
	}
	c := sharedHTTPClient()
	seen := map[string]bool{}
	for i := 0; i < 12; i++ {
		ua := c.UA()
		if ua != "UA-A" && ua != "UA-B" && ua != "UA-C" {
			t.Fatalf("UA() = %q, not from the injected pool", ua)
		}
		seen[ua] = true
	}
	if len(seen) != 3 {
		t.Fatalf("rotation did not cover the pool: %v", seen)
	}
}

func TestSharedHTTPClientUAPoolMissingFallsBackToBundled(t *testing.T) {
	// With the ua-pool.json compiled in via go:embed, a missing data/ file no
	// longer leaves the pool empty: LoadUAPoolDefault falls back to the bundled
	// 44-entry pool, so the shared client always rotates over a real pool.
	t.Setenv("MEDIAMON_UA_POOL", filepath.Join(t.TempDir(), "no-such-file.json"))
	if uas := uaPoolUserAgents(); len(uas) == 0 {
		t.Fatal("missing pool file should fall back to the bundled pool, got empty")
	}
	c := sharedHTTPClient()
	if c.UA() == "" {
		t.Fatal("client with bundled pool must rotate over a non-empty UA list")
	}
}

func TestAccountPoolForValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "accounts")
	t.Setenv("MEDIAMON_ACCOUNTS_DIR", dir)

	pool, err := accounts.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Save(accounts.Account{ID: "acc-1", Platform: "douyin", Cookies: map[string]string{"ttwid": "tok"}}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}

	// Empty id: no pool, no error (platform defaults kept).
	p, err := accountPoolFor("douyin", "")
	if err != nil || p != nil {
		t.Fatalf("empty id = (%v, %v)", p, err)
	}
	// Unknown id and platform mismatch fail closed.
	if _, err := accountPoolFor("douyin", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown id error = %v", err)
	}
	if _, err := accountPoolFor("kuaishou", "acc-1"); err == nil || !strings.Contains(err.Error(), "platform") {
		t.Fatalf("platform mismatch error = %v", err)
	}
	// Valid account resolves.
	p, err = accountPoolFor("douyin", "acc-1")
	if err != nil || p == nil {
		t.Fatalf("valid account = (%v, %v)", p, err)
	}
	// "auto" passes through with the pool (silent-scraping fix: the CLI used
	// to reject --account auto with "not found"; the engine's auto mode was
	// reachable only through mediad REST).
	p, err = accountPoolFor("douyin", "auto")
	if err != nil || p == nil {
		t.Fatalf("auto account = (%v, %v)", p, err)
	}
}
