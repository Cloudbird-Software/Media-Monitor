// wiring.go holds the mediad-mcp startup wiring that composes internal
// packages at the cmd layer: account-pool/UA-pool injection and the license
// gate. (Kept parallel to cmd/mediad/wiring.go; cmd packages cannot share
// code without a new internal package.)
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
	"github.com/Cloudbird-Software/Media-Monitor/internal/license"
)

// accountsDirEnv resolves the account pool dir: MEDIAMON_ACCOUNTS_DIR wins,
// default <dataDir>/accounts (same style as the MEDIAMON_*_COOKIES overrides).
func accountsDirEnv(dataDir string) string {
	if d := os.Getenv("MEDIAMON_ACCOUNTS_DIR"); d != "" {
		return d
	}
	return filepath.Join(dataDir, "accounts")
}

// uaPoolUserAgents loads the shared UA rotation pool via
// accounts.LoadUAPoolDefault (MEDIAMON_UA_POOL overrides the path; default is
// the executable-relative data/ua-pool.json). A missing/broken file is NOT an
// error: the UA pool is an enhancement, not a gate — nil keeps the HTTP
// client's built-in pool.
func uaPoolUserAgents() []string {
	pool, err := accounts.LoadUAPoolDefault(os.Getenv("MEDIAMON_UA_POOL"))
	if err != nil || pool == nil {
		return nil
	}
	path := os.Getenv("MEDIAMON_UA_POOL")
	if path == "" {
		if path, err = accounts.DefaultUAPoolPath(); err != nil {
			return nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		UAs []string `json:"uas"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.UAs) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "mediad-mcp: ua pool: %d user-agents loaded from %s\n", len(doc.UAs), path)
	return doc.UAs
}

// loadLicenseGate builds the cmd-layer license gate. The gate is ON by
// default and fails closed: without a verifiable license every gated tool
// (collect/action class) is denied, while meta surfaces (version,
// contracts_list, accounts_list, adapt_canary_offline) stay open. Setting
// MEDIAMON_LICENSE_REQUIRED=false explicitly disables the gate (dev-only,
// same spirit as --allow-unsigned).
func loadLicenseGate(dataDir string) (*license.Gate, string) {
	if strings.EqualFold(os.Getenv("MEDIAMON_LICENSE_REQUIRED"), "false") {
		return nil, "DISABLED via MEDIAMON_LICENSE_REQUIRED=false (dev-only; collect/action tools are not gated)"
	}
	dir := os.Getenv("MEDIAMON_LICENSE_DIR")
	if dir == "" {
		dir = filepath.Join(dataDir, "license")
	}
	raw, err := base64.StdEncoding.DecodeString(os.Getenv("MEDIAMON_LICENSE_PUBKEY"))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		g := license.NewGate(nil, nil, errors.New("MEDIAMON_LICENSE_PUBKEY missing or not a base64 Ed25519 public key"))
		return g, "enabled (fail-closed): MEDIAMON_LICENSE_PUBKEY missing/invalid, all gated tools denied"
	}
	g, err := license.LoadGate(dir, ed25519.PublicKey(raw), nil)
	if err != nil {
		g = license.NewGate(nil, nil, fmt.Errorf("license verifier: %w", err))
		return g, "enabled (fail-closed): verifier construction failed, all gated tools denied"
	}
	if _, ok := g.Active(); ok {
		return g, "enabled: license active (dir " + dir + ")"
	}
	return g, "enabled (fail-closed): no valid license in " + dir
}

// licenseDeniedErr renders a Gate denial as a structured tool error: the
// message is a JSON object the caller can parse for reason/detail.
func licenseDeniedErr(err error) error {
	reason := "unknown"
	detail := err.Error()
	var de *license.DeniedError
	if errors.As(err, &de) {
		reason = string(de.Reason)
		if de.Detail != "" {
			detail = de.Detail
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"error":  "license_denied",
		"reason": reason,
		"detail": detail,
	})
	return errors.New(string(raw))
}
