// Package kuaishou provides the default kuaishou assembly for the collect
// engine: contract names, UA/cookie hints and the signing hook surface.
package kuaishou

import (
	"context"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
)

// Platform is the canonical kuaishou platform key used in contracts and
// engine wiring.
const Platform = "kuaishou"

// mobileUA is the recommended primary User-Agent for the kuaishou web
// surface (android mobile web).
const mobileUA = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36"

// SignerFunc computes kuaishou request signature values (kuaishou.web.cp /
// did / clientid derived params). The returned kv map is merged into the
// final query by the collect engine.
type SignerFunc func(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error)

// Assembly is the kuaishou default assembly.
type Assembly struct {
	// Platform key.
	Platform string
	// UA is the recommended User-Agent for kuaishou endpoints.
	UA string
	// CookieNames documents the cookie names the kuaishou contracts require
	// (did for anonymous web reads).
	CookieNames []string
	// Names maps collect categories to contract names; "" = not declared.
	Names map[string]string
	// Signer is the injectable signing hook (nil = no signature decoration).
	Signer SignerFunc
}

// SignerAs adapts the hook to the httpclient.Signer interface (nil when unset).
func (d *Assembly) SignerAs() httpclient.Signer {
	if d == nil || d.Signer == nil {
		return nil
	}
	return httpclient.StaticSigner{Fn: d.Signer}
}

// Defaults loads the contract registry from contractsDir (caller-supplied
// adapt/contracts location) and returns the kuaishou assembly.
func Defaults(contractsDir string) (*Assembly, *contracts.Registry, error) {
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, contractsDir); err != nil {
		return nil, nil, err
	}
	return &Assembly{
		Platform:    Platform,
		UA:          mobileUA,
		CookieNames: []string{"did"},
		Names: map[string]string{
			"search":       "kuaishou-search",
			"comments":     "kuaishou-comments",
			"replies":      "",
			"user":         "kuaishou-user",
			"group":        "kuaishou-group-members",
			"send_message": "",
			// capability B (user discovery) + the ks leg of capability A's
			// observed walk (profile/feed fills the ks user_posts gap)
			"user_search": "kuaishou-user-search",
			"user_posts":  "kuaishou-profile-feed",
		},
	}, reg, nil
}
