// cmd/mediactl — live canary driver: golden checks against REAL platform
// endpoints, driven by secrets provisioned on the deployment (docs/CANARY.md).
// Never runs implicitly: only `mediactl adapt canary --live`.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/kuaishou"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/xhs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/signclient"
)

// liveCanary runs one golden pass against the live platforms. Each platform
// without a cookie secret is reported as skipped (documented expected skip),
// never as a silent green.
func liveCanary(reg *contracts.Registry) error {
	names := loadNames()
	cookies := map[string]string{}
	type plat struct{ name, cookieEnv string }
	plats := []plat{
		{douyin.Platform, "MEDIAMON_CANARY_COOKIES_DOUYIN"},
		{kuaishou.Platform, "MEDIAMON_CANARY_COOKIES_KUAISHOU"},
		{xhs.Platform, "MEDIAMON_CANARY_COOKIES_XHS"},
	}
	for _, p := range plats {
		if v := os.Getenv(p.cookieEnv); v != "" {
			cookies[p.name] = v
		}
	}

	signers := map[string]httpclient.Signer{}
	if u := os.Getenv("MEDIAMON_SIGNER_URL"); u != "" {
		sc := signclient.New(signclient.Config{BaseURL: u, Token: os.Getenv("MEDIAMON_SIGNER_TOKEN")})
		for _, p := range plats {
			signers[p.name] = sc
		}
	}

	eng := collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 20 * time.Second}),
		Obs:      obs.NewCounterMap(),
		Signers:  signers,
		Cookies:  cookies,
		Names:    names,
	})

	keyword := os.Getenv("MEDIAMON_CANARY_KEYWORD")
	if keyword == "" {
		keyword = "美食"
	}

	failures := 0
	for _, p := range plats {
		if cookies[p.name] == "" {
			fmt.Printf("live canary %-10s SKIPPED (no cookie secret %s)\n", p.name, p.cookieEnv)
			continue
		}
		items, cur, err := eng.SearchItems(context.Background(), p.name, keyword, "", model.Cursor{}, 5)
		if err != nil {
			fmt.Printf("live canary %-10s ERROR search: %v\n", p.name, err)
			failures++
			continue
		}
		fmt.Printf("live canary %-10s search ok: %d items (has_more=%v, keyword=%q)\n", p.name, len(items), cur.HasMore, keyword)
		for i := range items {
			if items[i].ID == "" {
				fmt.Printf("live canary %-10s WARN: item %d has no id (binding drift?)\n", p.name, i)
				failures++
			}
		}
		if len(items) == 0 {
			continue
		}
		comments, ccur, err := eng.ItemComments(context.Background(), p.name, items[0].ID, model.Cursor{}, 5)
		if err != nil {
			fmt.Printf("live canary %-10s ERROR comments(%s): %v\n", p.name, items[0].ID, err)
			failures++
			continue
		}
		users := 0
		for _, c := range comments {
			if c.User.UID != "" || c.User.SecUID != "" {
				users++
			}
		}
		fmt.Printf("live canary %-10s comments ok: %d comments (first item), %d with user identity, has_more=%v\n",
			p.name, len(comments), users, ccur.HasMore)
	}

	if failures > 0 {
		return fmt.Errorf("live canary: %d error-level findings (see above)", failures)
	}
	fmt.Println("live canary: platforms run without error-level findings")
	return nil
}

// loadNames maps scope -> contract names for the three platforms.
func loadNames() map[string]map[string]string {
	out := map[string]map[string]string{}
	out[douyin.Platform] = map[string]string{"search": "douyin-search", "comments": "douyin-comments"}
	out[kuaishou.Platform] = map[string]string{"search": "kuaishou-search", "comments": "kuaishou-comments"}
	out[xhs.Platform] = map[string]string{"search": "xhs-search", "comments": "xhs-comments"}
	return out
}
