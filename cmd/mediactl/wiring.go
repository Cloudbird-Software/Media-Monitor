package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/license"
)

// wiring.go — shared cmd-layer wiring: account-pool injection, the UA
// rotation pool for the shared HTTP client, and the license gate consulted by
// every collection/action command (collect, send, trace, live monitor).

// accountPoolFor opens the account pool and validates that account id exists
// and belongs to platform. An empty id returns (nil, nil): the caller keeps
// the platform-level defaults (backward compatible). The pool is left open on
// purpose — the collect engine reads from it for the process lifetime (the
// CLI is one-shot).
func accountPoolFor(platform, id string) (*accounts.Pool, error) {
	if id == "" {
		return nil, nil
	}
	pool, err := accounts.Open(accountsDir())
	if err != nil {
		return nil, err
	}
	a, ok := pool.Get(id)
	if !ok {
		return nil, fmt.Errorf("account %q not found in pool %s", id, accountsDir())
	}
	if a.Platform != platform {
		return nil, fmt.Errorf("account %q belongs to platform %q, not %q", id, a.Platform, platform)
	}
	return pool, nil
}

// uaPoolUserAgents loads the UA rotation pool for the shared HTTP client:
// $MEDIAMON_UA_POOL when set, otherwise the executable-relative
// data/ua-pool.json (accounts.LoadUAPoolDefault). The pool is an enhancement,
// not a gate: a missing/unreadable file returns nil and the client keeps its
// built-in rotation.
func uaPoolUserAgents() []string {
	pool, err := accounts.LoadUAPoolDefault(os.Getenv("MEDIAMON_UA_POOL"))
	if err != nil {
		return nil
	}
	// UAPool exposes rotation only; drain it until every entry was seen so the
	// HTTP client rotates over the full pool (bounded — duplicates are skipped).
	n := pool.Len()
	seen := map[string]bool{}
	out := make([]string, 0, n)
	for i := 0; i < n*8 && len(out) < n; i++ {
		if ua := pool.Next(); ua != "" && !seen[ua] {
			seen[ua] = true
			out = append(out, ua)
		}
	}
	return out
}

// sharedHTTPClient builds the HTTP client shared by the collect/send/live
// engines, with the UA pool injected when available.
func sharedHTTPClient() *httpclient.Client {
	return httpclient.New(httpclient.Config{UserAgents: uaPoolUserAgents()})
}

// licenseDir resolves the license directory: $MEDIAMON_LICENSE_DIR override,
// default data/license.
func licenseDir() string {
	if d := os.Getenv("MEDIAMON_LICENSE_DIR"); d != "" {
		return d
	}
	return filepath.Join("data", "license")
}

// licenseRequired reports whether the license gate is on. It defaults to ON
// (fail-closed); MEDIAMON_LICENSE_REQUIRED=false disables it explicitly —
// the same explicit-bypass convention as --allow-unsigned (dev-only).
func licenseRequired() bool {
	return !strings.EqualFold(os.Getenv("MEDIAMON_LICENSE_REQUIRED"), "false")
}

// licensePublicKey decodes the verifier public key from
// $MEDIAMON_LICENSE_PUBKEY (base64 ed25519 public key, 32 bytes). The signing
// private key never ships in this repo (docs/LICENSE-PROTOCOL.md), so the key
// is deployment-provided. Unset returns (nil, nil) — the caller fails closed.
func licensePublicKey() (ed25519.PublicKey, error) {
	b64 := strings.TrimSpace(os.Getenv("MEDIAMON_LICENSE_PUBKEY"))
	if b64 == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("MEDIAMON_LICENSE_PUBKEY: base64 decode: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("MEDIAMON_LICENSE_PUBKEY: bad key length %d, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// requireLicense enforces the license gate for a gated command (feature is a
// license feature flag: collect / dm / live). A denial is printed as a
// structured JSON line on stderr and returned so main exits non-zero.
func requireLicense(feature string) error {
	if !licenseRequired() {
		return nil
	}
	pub, err := licensePublicKey()
	if err != nil {
		return err
	}
	if pub == nil {
		derr := &license.DeniedError{Reason: license.ReasonMalformed, Detail: "no license public key configured (set MEDIAMON_LICENSE_PUBKEY) — gate is fail-closed"}
		printLicenseDenial(derr)
		return derr
	}
	gate, err := license.LoadGate(licenseDir(), pub, nil)
	if err != nil {
		return err
	}
	if derr := gate.Check(feature); derr != nil {
		var de *license.DeniedError
		if errors.As(derr, &de) {
			printLicenseDenial(de)
		}
		return derr
	}
	return nil
}

// printLicenseDenial prints the structured refusal (stable reason vocabulary
// from internal/license) to stderr.
func printLicenseDenial(de *license.DeniedError) {
	b, err := json.Marshal(de)
	if err != nil {
		fmt.Fprintf(os.Stderr, "license denied: %s\n", de.Reason)
		return
	}
	fmt.Fprintf(os.Stderr, "license denied: %s\n", b)
}
