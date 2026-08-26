package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/signclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
	"github.com/Cloudbird-Software/Media-Monitor/internal/tasks"
)

// cmdSend runs a direct-message broadcast job through the tasks.Sender
// (contract-driven engine + send-cap bookkeeping).
func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	platform := fs.String("platform", "", "platform: douyin|kuaishou|xhs")
	account := fs.String("account", "", "account id to act as (empty = platform default)")
	first := fs.String("first", "", "first message text (required; {nickname} substituted)")
	second := fs.String("second", "", "optional second message text")
	delay := fs.Int64("second-delay-ms", 15000, "delay before second message")
	cap := fs.Int("cap", 0, "max sends per account (0 = unlimited)")
	targets := fs.String("targets", "", "comma-separated target sec_uids")
	nickfile := fs.String("nickfile", "", "optional JSON file {\"sec_uid\":\"nickname\"} for {nickname} substitution")
	signerURL := fs.String("signer-url", os.Getenv("MEDIAMON_SIGNER_URL"), "remote signer base URL (default $MEDIAMON_SIGNER_URL)")
	cookieFile := fs.String("cookies", "", "cookie file (first line 'k1=v1; k2=v2')")
	dataDir := fs.String("data", filepath.Join("data", "tasks"), "store dir for send-cap counters")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *platform == "" || *first == "" || *targets == "" {
		return fmt.Errorf("--platform, --first and --targets are required")
	}
	if err := requireLicense("dm"); err != nil {
		return err
	}
	eng, err := buildSendEngine(*platform, *cookieFile, *signerURL, *account)
	if err != nil {
		return err
	}
	st, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	var nicks map[string]string
	if *nickfile != "" {
		raw, err := os.ReadFile(*nickfile)
		if err != nil {
			return fmt.Errorf("nickfile: %w", err)
		}
		if err := json.Unmarshal(raw, &nicks); err != nil {
			return fmt.Errorf("nickfile: %w", err)
		}
	}
	cfg := tasks.SendTaskConfig{
		Platform:       *platform,
		AccountID:      *account,
		Targets:        splitAndTrim(*targets, ","),
		FirstMessage:   tasks.MessageTemplate{Content: *first},
		SecondDelayMs:  *delay,
		SendCap:        *cap,
		SubstituteNick: nicks,
	}
	if *second != "" {
		cfg.SecondMessage = &tasks.MessageTemplate{Content: *second}
	}
	rep, err := tasks.NewSender(eng, st).Run(context.Background(), cfg)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// buildSendEngine assembles the collect engine scoped to the platform's
// send_message contract. accountID ("" = platform default) routes every send
// through the account's cookie/proxy/UA.
func buildSendEngine(platform, cookieFile, signerURL, accountID string) (*collect.Engine, error) {
	switch platform {
	case "douyin", "kuaishou", "xhs":
	default:
		return nil, fmt.Errorf("--platform must be one of douyin|kuaishou|xhs, got %q", platform)
	}
	cdir := filepath.Join(adaptDir(), "contracts")
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, cdir); err != nil {
		return nil, err
	}
	names := map[string]map[string]string{platform: {"send_message": platform + "-send-message"}}

	cookies := map[string]string{}
	if cookieFile != "" {
		hdr, err := loadCookieHeader(cookieFile)
		if err != nil {
			return nil, err
		}
		cookies[platform] = hdr
	}
	signers := map[string]httpclient.Signer{}
	if signerURL != "" {
		sc := signclient.New(signclient.Config{BaseURL: signerURL})
		signers[platform] = sc
	}
	pool, err := accountPoolFor(platform, accountID)
	if err != nil {
		return nil, err
	}
	return collect.New(collect.Context{
		Registry:  reg,
		HTTP:      sharedHTTPClient(),
		Obs:       obs.NewCounterMap(),
		Cookies:   cookies,
		Signers:   signers,
		Names:     names,
		Accounts:  pool,
		AccountID: accountID,
	}), nil
}
