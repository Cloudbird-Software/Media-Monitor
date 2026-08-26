package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/license"
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
}

// writeSignedLicense creates a license file bound to this machine, signed by
// a fresh key, and returns the base64 public key.
func writeSignedLicense(t *testing.T, dir string, features []string) string {
	t.Helper()
	pub, priv, err := license.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	lic := license.License{
		Machine:   license.MachineFingerprint(),
		NotBefore: now - 60,
		NotAfter:  now + 3600,
		Features:  features,
		Issuer:    "test",
	}
	sig, err := license.Sign(priv, lic)
	if err != nil {
		t.Fatal(err)
	}
	lic.Signature = sig
	raw, err := json.Marshal(lic)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, license.LicenseFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func TestRequireLicenseDisabledByEnv(t *testing.T) {
	t.Setenv("MEDIAMON_LICENSE_REQUIRED", "false")
	// No pubkey, no license dir: the explicit bypass wins.
	if err := requireLicense("collect"); err != nil {
		t.Fatalf("disabled gate denied: %v", err)
	}
}

func TestRequireLicenseNoPubkeyFailsClosed(t *testing.T) {
	t.Setenv("MEDIAMON_LICENSE_PUBKEY", "")
	err := requireLicense("collect")
	var de *license.DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("error = %v, want *license.DeniedError", err)
	}
	if de.Reason != license.ReasonMalformed {
		t.Fatalf("reason = %q, want %q", de.Reason, license.ReasonMalformed)
	}
}

func TestRequireLicenseNoLicenseFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEDIAMON_LICENSE_DIR", dir)
	pub, _, err := license.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEDIAMON_LICENSE_PUBKEY", base64.StdEncoding.EncodeToString(pub))

	err = requireLicense("collect")
	var de *license.DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("error = %v, want *license.DeniedError", err)
	}
	if de.Reason != license.ReasonNoLicense {
		t.Fatalf("reason = %q, want %q", de.Reason, license.ReasonNoLicense)
	}
}

func TestRequireLicenseSignedLicensePasses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEDIAMON_LICENSE_DIR", dir)
	pubB64 := writeSignedLicense(t, dir, []string{"collect", "dm", "live"})
	t.Setenv("MEDIAMON_LICENSE_PUBKEY", pubB64)

	if err := requireLicense("collect"); err != nil {
		t.Fatalf("signed license denied: %v", err)
	}
	// A feature the license does not enable is denied with feature_disabled.
	err := requireLicense("vision")
	var de *license.DeniedError
	if !errors.As(err, &de) || de.Reason != license.ReasonFeature {
		t.Fatalf("feature check = %v, want feature_disabled", err)
	}
}
