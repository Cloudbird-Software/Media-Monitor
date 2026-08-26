package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
)

// bytesReader returns a *bytes.Reader over raw (cookie import helpers take
// io.Reader).
func bytesReader(raw []byte) *bytes.Reader { return bytes.NewReader(raw) }

// splitAndTrim splits s on sep and trims each piece.
func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// accountsDir resolves the account pool directory: MEDIAMON_ACCOUNTS_DIR
// override, default "data/accounts" relative to CWD.
func accountsDir() string {
	if d := os.Getenv("MEDIAMON_ACCOUNTS_DIR"); d != "" {
		return d
	}
	return filepath.Join("data", "accounts")
}

func cmdAccounts(args []string) error {
	fs := flag.NewFlagSet("accounts", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "use: accounts import|export|list|delete (see mediactl help)\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("missing accounts subcommand")
	}
	switch fs.Arg(0) {
	case "import":
		return accountsImport(fs.Args()[1:])
	case "export":
		return accountsExport(fs.Args()[1:])
	case "list":
		return accountsList(fs.Args()[1:])
	case "delete":
		return accountsDelete(fs.Args()[1:])
	default:
		return fmt.Errorf("unknown accounts subcommand %q", fs.Arg(0))
	}
}

func accountsImport(args []string) error {
	fs := flag.NewFlagSet("accounts import", flag.ExitOnError)
	platform := fs.String("platform", "", "platform: douyin|kuaishou|xhs")
	format := fs.String("format", "netscape", "cookie file format: netscape|json")
	file := fs.String("file", "", "cookie file to import (netscape cookie.txt or JSON)")
	id := fs.String("id", "", "account id (empty = auto-generate)")
	nickname := fs.String("nickname", "", "optional display name")
	proxy := fs.String("proxy", "", "optional per-account proxy (http://user:pass@host:port)")
	ua := fs.String("ua", "", "optional pinned User-Agent")
	tags := fs.String("tags", "", "comma-separated tags")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *platform == "" || *file == "" {
		return fmt.Errorf("--platform and --file are required")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("read %s: %w", *file, err)
	}
	var cookies map[string]string
	switch *format {
	case "netscape":
		cookies, err = accounts.ImportCookiesNetscape(bytesReader(raw))
	case "json":
		cookies, err = accounts.ImportCookiesJSON(bytesReader(raw))
	default:
		return fmt.Errorf("unknown format %q (use netscape|json)", *format)
	}
	if err != nil {
		return fmt.Errorf("import cookies: %w", err)
	}
	if *id == "" {
		*id = fmt.Sprintf("%s-%d", *platform, os.Getpid())
	}
	acct := accounts.Account{
		ID:       *id,
		Platform: *platform,
		Nickname: *nickname,
		Cookies:  cookies,
		Proxy:    *proxy,
		UA:       *ua,
		Tags:     splitTags(*tags),
		Status:   accounts.StatusActive,
	}
	pool, err := accounts.Open(accountsDir())
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Save(acct); err != nil {
		return err
	}
	fmt.Printf("imported account %q (%s, %d cookies)\n", acct.ID, acct.Platform, len(cookies))
	return nil
}

func accountsExport(args []string) error {
	fs := flag.NewFlagSet("accounts export", flag.ExitOnError)
	id := fs.String("id", "", "account id (required)")
	format := fs.String("format", "netscape", "export format: netscape|json")
	file := fs.String("file", "", "output file (required)")
	domain := fs.String("domain", ".douyin.com", "cookie domain for netscape export")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *file == "" {
		return fmt.Errorf("--id and --file are required")
	}
	pool, err := accounts.Open(accountsDir())
	if err != nil {
		return err
	}
	defer pool.Close()
	acct, ok := pool.Get(*id)
	if !ok {
		return fmt.Errorf("account %q not found", *id)
	}
	f, err := os.Create(*file)
	if err != nil {
		return err
	}
	defer f.Close()
	switch *format {
	case "netscape":
		err = accounts.ExportCookiesNetscape(f, *domain, acct.Cookies)
	case "json":
		err = accounts.ExportCookiesJSON(f, acct.Cookies)
	default:
		return fmt.Errorf("unknown format %q (use netscape|json)", *format)
	}
	if err != nil {
		return err
	}
	fmt.Printf("exported account %q (%d cookies) -> %s\n", acct.ID, len(acct.Cookies), *file)
	return nil
}

func accountsList(args []string) error {
	fs := flag.NewFlagSet("accounts list", flag.ExitOnError)
	platform := fs.String("platform", "", "filter by platform")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pool, err := accounts.Open(accountsDir())
	if err != nil {
		return err
	}
	defer pool.Close()
	for _, a := range pool.List() {
		if *platform != "" && a.Platform != *platform {
			continue
		}
		fmt.Printf("%s\t%s\t%s\tcookies=%d\tproxy=%q\tstatus=%s\n", a.ID, a.Platform, a.Nickname, len(a.Cookies), a.Proxy, a.Status)
	}
	return nil
}

func accountsDelete(args []string) error {
	fs := flag.NewFlagSet("accounts delete", flag.ExitOnError)
	id := fs.String("id", "", "account id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("--id is required")
	}
	pool, err := accounts.Open(accountsDir())
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Delete(*id); err != nil {
		return err
	}
	fmt.Printf("deleted account %q\n", *id)
	return nil
}

func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	parts := []string{}
	for _, p := range splitAndTrim(s, ",") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}
