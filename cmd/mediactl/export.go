package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/datacenter"
)

// cmdExport runs a data export (CSV) with optional keyword filtering.
// The hub reads back the persisted store (datacenter.New replays the
// collection), so an export over a dir written by a previous process
// produces the real rows — not a permanent 0-row file.
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	format := fs.String("format", "csv", "export format: csv")
	dataDir := fs.String("data", filepath.Join("data", "datacenter"), "datacenter store dir")
	keywords := fs.String("filter", "", "comma-separated keywords")
	matchAll := fs.Bool("match-all", false, "require ALL keywords (default: any)")
	out := fs.String("out", "", "output file (default: stdout)")
	platform := fs.String("platform", "", "filter by platform")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h, err := datacenter.New(datacenter.Config{Dir: *dataDir})
	if err != nil {
		return err
	}
	defer h.Close()
	var kws []string
	if *keywords != "" {
		kws = splitAndTrim(*keywords, ",")
	}
	records := h.List(kws, !*matchAll)
	if *platform != "" {
		filtered := records[:0]
		for _, r := range records {
			if r.Platform == *platform {
				filtered = append(filtered, r)
			}
		}
		records = filtered
	}
	switch *format {
	case "csv":
		w := os.Stdout
		if *out != "" {
			f, err := os.Create(*out)
			if err != nil {
				return err
			}
			defer f.Close()
			w = f
		}
		if err := datacenter.WriteCSV(w, records, nil, false); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "export: %d record(s) from %s\n", len(records), *dataDir)
		return nil
	default:
		return fmt.Errorf("unknown format %q", *format)
	}
}

// cmdWebhook manages the datacenter webhook (test/retry).
//
// The webhook URL resolution is --url > MEDIAMON_WEBHOOK_URL (the same env
// mediad consumes); retry reports the queue size, delivered count and
// still-failing count, and exits non-zero while records remain failing —
// a retry pass can no longer silently report zero while doing nothing
// (report-S2 defect ③).
func cmdWebhook(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use: webhook test|retry --data <dir> [--url <webhook>]")
	}
	fs := flag.NewFlagSet("webhook", flag.ExitOnError)
	dataDir := fs.String("data", filepath.Join("data", "datacenter"), "datacenter store dir")
	urlFlag := fs.String("url", os.Getenv("MEDIAMON_WEBHOOK_URL"), "webhook URL (default: $MEDIAMON_WEBHOOK_URL)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *urlFlag == "" {
		return fmt.Errorf("webhook url required: pass --url or set MEDIAMON_WEBHOOK_URL (retry without a target used to report 0 while doing nothing)")
	}
	h, err := datacenter.New(datacenter.Config{Dir: *dataDir, WebhookURL: *urlFlag})
	if err != nil {
		return err
	}
	defer h.Close()
	switch args[0] {
	case "test":
		if err := h.TestWebhook(context.Background()); err != nil {
			return fmt.Errorf("webhook test failed: %w", err)
		}
		fmt.Println("webhook test OK")
		return nil
	case "retry":
		outcome, err := h.RetryFailed(context.Background())
		if err != nil {
			if errors.Is(err, datacenter.ErrNoWebhook) {
				return fmt.Errorf("webhook retry: no webhook url configured (--url / MEDIAMON_WEBHOOK_URL)")
			}
			return fmt.Errorf("webhook retry: %w", err)
		}
		fmt.Printf("webhook retry: %d queued, %d re-pushed OK, %d still failing\n",
			outcome.Queued, outcome.Repushed, outcome.StillFailing)
		if outcome.StillFailing > 0 {
			return fmt.Errorf("webhook retry: %d record(s) still failing (kept in the retry queue)", outcome.StillFailing)
		}
		return nil
	default:
		return fmt.Errorf("unknown webhook subcommand %q", args[0])
	}
}

var _ = strings.Join
