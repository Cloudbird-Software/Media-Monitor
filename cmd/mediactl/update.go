package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Cloudbird-Software/Media-Monitor/internal/selfupdate"
)

// update.go — `mediactl update check`: manifest-driven self-update check and
// (optional) download. The library verifies SHA256 and never touches the
// running binary; a checksum mismatch discards the download. This surface is
// (like version).

// updatesDir resolves the update download dir: $MEDIAMON_UPDATES_DIR
// override, default data/updates.
func updatesDir() string {
	if d := os.Getenv("MEDIAMON_UPDATES_DIR"); d != "" {
		return d
	}
	return filepath.Join("data", "updates")
}

func cmdUpdate(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use: update check --manifest-url <url> [--download] [--dest <dir>]")
	}
	switch args[0] {
	case "check":
		return updateCheck(args[1:])
	default:
		return fmt.Errorf("unknown update subcommand %q", args[0])
	}
}

func updateCheck(args []string) error {
	fs := flag.NewFlagSet("update check", flag.ExitOnError)
	manifestURL := fs.String("manifest-url", os.Getenv("MEDIAMON_UPDATE_MANIFEST_URL"), "update manifest URL (default: $MEDIAMON_UPDATE_MANIFEST_URL)")
	download := fs.Bool("download", false, "download the update (SHA256-verified) into the updates dir")
	dest := fs.String("dest", "", "download dir (default: $MEDIAMON_UPDATES_DIR or data/updates)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifestURL == "" {
		return fmt.Errorf("--manifest-url (or $MEDIAMON_UPDATE_MANIFEST_URL) is required")
	}
	dir := *dest
	if dir == "" {
		dir = updatesDir()
	}
	checker := selfupdate.NewChecker(*manifestURL, version, sharedHTTPClient())
	m, err := checker.Check()
	if err != nil {
		return err
	}
	fmt.Printf("current version: %s\n", version)
	if m == nil {
		fmt.Println("already up to date")
		return nil
	}
	fmt.Printf("update available: %s\n", m.Version)
	if m.ReleaseNotes != "" {
		fmt.Printf("release notes: %s\n", m.ReleaseNotes)
	}
	if !*download {
		fmt.Println("re-run with --download to fetch it")
		return nil
	}
	path, err := checker.Download(m, dir)
	if err != nil {
		return err
	}
	fmt.Printf("downloaded (sha256 verified): %s\n", path)
	return nil
}
