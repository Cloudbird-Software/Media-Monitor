// wiring.go holds the mediad-mcp startup wiring that composes internal
// packages at the cmd layer: account-pool/UA-pool injection. (Kept
// parallel to cmd/mediad/wiring.go; cmd packages cannot share code without a
// new internal package.)
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
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
