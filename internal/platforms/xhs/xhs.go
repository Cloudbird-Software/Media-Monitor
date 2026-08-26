// Package xhs provides the default xiaohongshu (xhs) assembly for the
// collect engine: contract names, UA/cookie hints and the signing hook
// surface.
package xhs

import (
	"context"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
)

// Platform is the canonical xhs platform key used in contracts and engine
// wiring.
const Platform = "xhs"

// mobileUA is the recommended primary User-Agent for the xiaohongshu web
// surface.
const mobileUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"

// SignerFunc computes xhs request signature values (x-s, x-t and friends).
// The returned kv map is merged into the final query by the collect engine.
type SignerFunc func(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error)

// Assembly is the xhs default assembly.
type Assembly struct {
	// Platform key.
	Platform string
	// UA is the recommended User-Agent for xhs endpoints.
	UA string
	// CookieNames documents the cookie names the xhs contracts require
	// (web_session for anonymous web reads).
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
// adapt/contracts location) and returns the xhs assembly.
func Defaults(contractsDir string) (*Assembly, *contracts.Registry, error) {
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, contractsDir); err != nil {
		return nil, nil, err
	}
	return &Assembly{
		Platform:    Platform,
		UA:          mobileUA,
		CookieNames: []string{"web_session"},
		Names: map[string]string{
			"search":       "xhs-search",
			"comments":     "xhs-comments",
			"replies":      "xhs-comments-replies",
			"user":         "xhs-user",
			"group":        "xhs-group-members",
			"send_message": "",
			"user_posts":   "xhs-user-notes",
		},
	}, reg, nil
}
