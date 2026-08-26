package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/datacenter"
)

// cmdExport runs a data export (CSV) with optional keyword filtering.
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
		return datacenter.WriteCSV(w, records, nil, false)
	default:
		return fmt.Errorf("unknown format %q", *format)
	}
}

// cmdWebhook manages the datacenter webhook (test/retry).
func cmdWebhook(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use: webhook test|retry --data <dir>")
	}
	fs := flag.NewFlagSet("webhook", flag.ExitOnError)
	dataDir := fs.String("data", filepath.Join("data", "datacenter"), "datacenter store dir")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	h, err := datacenter.New(datacenter.Config{Dir: *dataDir})
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
		failing, err := h.RetryFailed(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("retry: %d records still failing\n", failing)
		return nil
	default:
		return fmt.Errorf("unknown webhook subcommand %q", args[0])
	}
}

var _ = strings.Join
