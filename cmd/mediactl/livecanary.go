// cmd/mediactl — live canary driver: golden checks against REAL platform
// endpoints, driven by secrets provisioned on the deployment (docs/CANARY.md).
// Never runs implicitly: only `mediactl adapt canary --live`.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/waterlevel"

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
	drifts := []liveDrift{}
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

		// Pagination-depth assertion (W7-C1 AC-5): fetch page 2 of the
		// comments chain — the half-dead-cookie shape (f2 #435) answers
		// 200 with an EMPTY page here while page 1 still looked alive.
		if ccur.HasMore {
			page2, _, derr := eng.ItemComments(context.Background(), p.name, items[0].ID, ccur, 5)
			switch {
			case derr != nil:
				fmt.Printf("live canary %-10s DRIFT depth: page-2 fetch error: %v\n", p.name, derr)
				drifts = append(drifts, liveDrift{Platform: p.name, Kind: "pagination-depth", Detail: derr.Error(), Account: maskAccount(p.name)})
				failures++
			case len(page2) == 0:
				fmt.Printf("live canary %-10s DRIFT depth: page 2 is EMPTY (200 + empty body — half-dead cookie)\n", p.name)
				drifts = append(drifts, liveDrift{Platform: p.name, Kind: "pagination-depth",
					Detail: "page 2 empty (200 + empty body)", Account: maskAccount(p.name)})
				failures++
			default:
				fmt.Printf("live canary %-10s depth ok: page 2 returned %d comments\n", p.name, len(page2))
			}
		}
	}

	// Drift artifacts: report file + (when the App secret exists) a
	// type:drift issue carrying the drift JSON and masked account ids.
	if len(drifts) > 0 {
		writeLiveDriftReport(drifts)
		fileDriftIssue(drifts)
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

// liveDrift is one error-level finding of a live run.
type liveDrift struct {
	Platform string `json:"platform"`
	Kind     string `json:"kind"` // search|comments|pagination-depth
	Detail   string `json:"detail"`
	// Account is masked by construction: platform + role only, never any
	// cookie fragment (INV-6).
	Account string `json:"account"`
}

// maskAccount renders the masked account identity for drift artifacts.
func maskAccount(platform string) string { return "canary-account(" + platform + ")" }

// writeLiveDriftReport persists the drift JSON under adapt/reports/.
func writeLiveDriftReport(drifts []liveDrift) {
	name := fmt.Sprintf("adapt/reports/live-canary-drift-%s.json", time.Now().UTC().Format("20060102-150405"))
	b, err := json.MarshalIndent(drifts, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll("adapt/reports", 0o755); err != nil {
		return
	}
	if err := os.WriteFile(name, b, 0o644); err == nil {
		fmt.Printf("live canary: drift report written: %s\n", name)
	}
}

// fileDriftIssue opens a type:drift issue via the App secret when present;
// without the secret the skip is printed (documented, never silent).
func fileDriftIssue(drifts []liveDrift) {
	if os.Getenv("AGENT_APP_SECRET") == "" {
		fmt.Println("live canary: drift issue filing SKIPPED (no AGENT_APP_SECRET — documented skip)")
		return
	}
	b, err := json.MarshalIndent(drifts, "", "  ")
	if err != nil {
		return
	}
	title := fmt.Sprintf("drift: live canary 红灯——%d 项发现（%s UTC）", len(drifts), time.Now().UTC().Format("2006-01-02 15:04"))
	body := fmt.Sprintf("## live canary drift\n\n```json\n%s\n```\n\n账号：脱敏 id 见各行 account 字段（零 cookie 片段，INV-6）；网络痕迹：本次为契约请求-断言链（无 HAR 附件时以 drift JSON 为准）。\n", b)
	num, err := waterlevel.CreateDriftIssueFull("154584760", "Cloudbird-Software/Media-Monitor", title, body)
	if err != nil {
		fmt.Printf("live canary: drift issue filing FAILED: %v\n", err)
		return
	}
	fmt.Printf("live canary: drift issue #%d opened\n", num)
}
