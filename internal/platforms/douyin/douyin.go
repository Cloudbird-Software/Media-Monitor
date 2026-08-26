// Package douyin provides the default douyin assembly for the collect
// engine: contract names, UA/cookie hints and the signing hook surface
// (a_bogus / msToken / X-Bogus are produced by an injected signer and merged
// into the outgoing query by the engine).
package douyin

import (
	"context"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
)

// Platform is the canonical douyin platform key used in contracts and engine
// wiring.
const Platform = "douyin"

// desktopUA is the recommended primary User-Agent for the douyin web
// surface (desktop Chrome).
const desktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// SignerFunc computes douyin request signature values (a_bogus, msToken,
// X-Bogus). The returned kv map is merged into the final query by the
// collect engine. Signature matches httpclient.Signer for direct injection.
type SignerFunc func(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error)

// Assembly is the douyin default assembly.
type Assembly struct {
	// Platform key.
	Platform string
	// UA is the recommended User-Agent for douyin endpoints.
	UA string
	// CookieNames documents the cookie names the douyin contracts require
	// (ttwid for anonymous reads, sessionid for account-bound surfaces).
	CookieNames []string
	// Names maps collect categories (search/comments/replies/user/group) to
	// contract names; "" means the category has no declared contract yet.
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

// Defaults loads the contract registry from contractsDir (the caller passes
// the adapt/contracts location — relative paths are unreliable once the
// binary runs elsewhere) and returns the douyin assembly.
func Defaults(contractsDir string) (*Assembly, *contracts.Registry, error) {
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, contractsDir); err != nil {
		return nil, nil, err
	}
	return &Assembly{
		Platform:    Platform,
		UA:          desktopUA,
		CookieNames: []string{"ttwid", "sessionid"},
		Names: map[string]string{
			"search":       "douyin-search",
			"comments":     "douyin-comments",
			"replies":      "douyin-comments-replies",
			"user":         "douyin-user",
			"group":        "douyin-group-members",
			"send_message": "douyin-send-message",
		},
	}, reg, nil
}
