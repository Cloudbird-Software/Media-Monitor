// Command mediactl is the Media-Monitor CLI: contract listing, offline
// canary/diff of the adaptation harness, contract-driven collection
// (collect), direct-message send, trace, live monitor, task ops, toolbox,
// netcapture query/export, and self-update check.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adapt"
	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/kuaishou"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/xhs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/signclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

// version is the running binary version reported by `version` and compared
// against the update manifest. A var so tests can pin it.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version":
		err = cmdVersion(os.Args[2:])
	case "contracts":
		err = cmdContracts(os.Args[2:])
	case "adapt":
		err = cmdAdapt(os.Args[2:])
	case "collect":

		err = cmdCollect(os.Args[2:])
	case "live":
		err = cmdLive(os.Args[2:])
	case "tasks":
		err = cmdTasks(os.Args[2:])
	case "upstream":
		err = cmdUpstream(os.Args[2:])
	case "accounts":
		err = cmdAccounts(os.Args[2:])
	case "vision":
		err = cmdVision(os.Args[2:])
	case "lab":
		err = cmdLab(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "trace":
		err = cmdTrace(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "webhook":
		err = cmdWebhook(os.Args[2:])
	case "netcapture":
		err = cmdNetcapture(os.Args[2:])
	case "update":
		err = cmdUpdate(os.Args[2:])
	case "toolbox":
		err = cmdToolbox(os.Args[2:])
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `mediactl — media-monitor command line

usage: mediactl <command> [flags]

commands:
  version                          print version
  update check --manifest-url <url> [--download] [--dest <dir>]
                                   check the update manifest (default url:
                                   $MEDIAMON_UPDATE_MANIFEST_URL); --download
                                   fetches the new binary (SHA256-verified by
                                   the library) into the updates dir
                                   (default $MEDIAMON_UPDATES_DIR or data/updates)
  contracts list                   list registered platform contracts
  adapt canary --offline [name]    run adaptation canaries (fixtures only;
                                   add --live when live driver exists)
  adapt diff --contract <name> --fixture <file> [--kind kind]
                                   diff one contract against one payload
  adapt snapshot --accept <name>   promote a canary's fixture to the next
                                   golden fixture sequence <contract>.<n>.json
                                   (old fixtures are kept; review diff first)
  collect search --platform <p> --keyword <k> [--type video|image] [--limit N]
  collect comments --platform <p> --item <id> [--limit N]
  collect replies --platform <p> --item <id> --cid <c> [--limit N]
  collect user --platform <p> --sec-uid <s>
  collect group --platform <p> --group <g> [--limit N]
  collect collects --platform <p> [--limit N]
                                   list bookmark/collects folders (paginated)
  collect collects-videos --platform <p> --folder-id <id> [--limit N]
                                   list the videos inside one collects folder
  collect video --platform <p> (--url <u> | --aweme-id <id>) [--download]
                                   resolve an item's watermark-free play URL
                                   and cover; --download streams it to
                                   --out-dir/<aweme_id>.mp4
  collect im-unread --platform <p> print the IM unread count + conversations
                                   contract-driven collection over the live
                                   platform endpoints (NDJSON on stdout);
                                   --cookies <file> (first line 'k1=v1; k2=v2'),
                                   --account <id> routes the request through
                                   the account's cookie/proxy/UA,
                                   --out-dir <dir> appends to a JSONL store
  live monitor --room <url>         watch a live room, print NDJSON events
  tasks submit --kind <k> --config <json>   submit a task to the local store
  tasks list --data <dir>          list tasks from the local store
  upstream scan [--window-hours N] [--out FILE]
                                   poll GitHub commit activity against the
                                   upstream/registry.json tracked paths and
                                   write a JSON summary plus stdout digest
  accounts import --platform <p> --file <cookie-file> [--format netscape|json]
                                   [--proxy <url>] [--ua <ua>] [--tags a,b]
                                   import cookies into a new account
  accounts export --id <id> --file <out> [--format netscape|json] [--domain d]
                                   export an account's cookies
  accounts list [--platform <p>]   list accounts in the pool
  vision run --goal <g> --serial <s> [--max-steps N] [--distill <flow.json>]
                                   vision-driven device run (MEDIAMON_VISION_ENDPOINT required)
  accounts delete --id <id>        remove an account
  send --platform <p> --first <text> --targets <sec_uid,...> [--second <text>]
                                   [--second-delay-ms N] [--cap N] [--account <id>]
                                   [--nickfile <json>] [--cookies <file>]
                                   broadcast direct messages (contract-driven;
                                   {nickname} substituted from --nickfile)
  trace run --platform <p> --targets <sec_uid,...> [--flow <file>] [--adb <addr>]
                                   [--account <id>] [--dm-first <text>] [--dm-second <text>]
                                   run a probabilistic trace/engagement sequence
                                   across adb devices (equalized); the profile
                                   deep link comes from the flow's
                                   profile_url_template (fail-closed when
                                   missing); DM reuses M2
  toolbox encrypt embed --text <t> [--secret <s>] [--min N] [--max M]
                                   hide text in zero-width characters (text
                                   from --text or stdin; --secret seeds the
                                   pattern deterministically)
  toolbox encrypt extract [--text <t>]
                                   strip zero-width characters (stdin ok)
  toolbox stylize (--phone <num> | --phones-file <f>) [--style] [--separator]
                                   stylize phone digits (--style = 固定风格,
                                   one fixed style per number)
  toolbox wechat-multi --num N [--helper-path <exe>]
                                   launch N WeChat instances via the bundled
                                   openwechat.exe helper
  netcapture list                  list persisted capture sessions
  netcapture export --project <name> --out <file.har>
                                   export one session as HAR; sessions are
                                   recorded by mediad / programmatic writers —
                                   this command only queries and exports
                                   (no CDP capture in a headless CLI)
  export --format csv --data <dir> [--filter <kws>] [--match-all] [--platform <p>] [--out <file>]
                                   export datacenter records (CSV) with keyword filter
  webhook test|retry --data <dir>  test a webhook endpoint or retry failed pushes

environment:
  MEDIAMON_ADAPT_DIR        adapt dir (default ./adapt)
  MEDIAMON_ACCOUNTS_DIR     account pool dir (default data/accounts)
  MEDIAMON_UA_POOL          UA pool file (default <exe>/data/ua-pool.json)
  MEDIAMON_NETCAPTURE_DIR   netcapture store dir (default data/netcapture)
  MEDIAMON_DATA_DIR         task store dir (default ./data)
  MEDIAMON_SIGNER_URL / MEDIAMON_SIGNER_TOKEN   remote signer service
`)
}

func cmdVersion(args []string) error {
	fmt.Println("mediactl version", version)
	return nil
}

func adaptDir() string {
	if d := os.Getenv("MEDIAMON_ADAPT_DIR"); d != "" {
		return d
	}
	return "adapt"
}

func loadRegistry() (*contracts.Registry, *adapt.Runner, error) {
	dir := adaptDir()
	reg := contracts.NewRegistry()
	cdir := filepath.Join(dir, "contracts")
	if _, err := os.Stat(cdir); err != nil {
		return nil, nil, fmt.Errorf("contracts dir %s: %w", cdir, err)
	}
	if err := contracts.LoadDir(reg, cdir); err != nil {
		return nil, nil, err
	}
	return reg, adapt.NewRunner(reg, filepath.Join(dir, "fixtures"), filepath.Join(dir, "canaries")), nil
}

func cmdContracts(args []string) error {
	fs := flag.NewFlagSet("contracts", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() > 0 && fs.Arg(0) != "list" {
		return fmt.Errorf("unknown contracts subcommand %q", fs.Arg(0))
	}
	reg, _, err := loadRegistry()
	if err != nil {
		return err
	}
	for _, name := range reg.List() {
		fmt.Println(name)
	}
	return nil
}

func cmdAdapt(args []string) error {
	fs := flag.NewFlagSet("adapt", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), "use: adapt canary|diff|snapshot (see mediactl help)\n") }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("missing adapt subcommand")
	}
	switch fs.Arg(0) {
	case "canary":
		return adaptCanary(fs.Args()[1:])
	case "diff":
		return adaptDiff(fs.Args()[1:])
	case "snapshot":
		return adaptSnapshot(fs.Args()[1:])
	default:
		return fmt.Errorf("unknown adapt subcommand %q", fs.Arg(0))
	}
}

func adaptCanary(args []string) error {
	fs := flag.NewFlagSet("adapt canary", flag.ExitOnError)
	offline := fs.Bool("offline", false, "run fixture-based canaries (no network)")
	live := fs.Bool("live", false, "run live canaries (requires secrets/env; inert until the live driver lands)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *offline == *live {
		return fmt.Errorf("exactly one of --offline / --live is required")
	}
	if *live {
		reg, _, err := loadRegistry()
		if err != nil {
			return err
		}
		return liveCanary(reg)
	}
	_, runner, err := loadRegistry()
	if err != nil {
		return err
	}
	reports, err := runner.RunAllOffline()
	if err != nil {
		return err
	}
	fmt.Print(contracts.Summarize(reports))
	for _, r := range reports {
		if !r.Healthy() {
			return fmt.Errorf("canary reports contain errors (see above)")
		}
	}
	fmt.Printf("offline canary: %d cases healthy\n", len(reports))
	return nil
}

func adaptDiff(args []string) error {
	fs := flag.NewFlagSet("adapt diff", flag.ExitOnError)
	name := fs.String("contract", "", "contract name")
	fixture := fs.String("fixture", "", "JSON payload file to diff against")
	kind := fs.String("kind", "", "items|comments|users|members (empty = all bindings)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *fixture == "" {
		return fmt.Errorf("--contract and --fixture are required")
	}
	reg, _, err := loadRegistry()
	if err != nil {
		return err
	}
	c, ok := reg.Get(*name)
	if !ok {
		return fmt.Errorf("contract %q not registered", *name)
	}
	raw, err := os.ReadFile(*fixture)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	rep := contracts.Diff(c, doc, *kind)
	rep.Observed = *fixture
	fmt.Print(contracts.Summarize([]*contracts.DiffReport{rep}))
	if !rep.Healthy() {
		return fmt.Errorf("diff contains errors")
	}
	return nil
}

func adaptSnapshot(args []string) error {
	fs := flag.NewFlagSet("adapt snapshot", flag.ExitOnError)
	name := fs.String("accept", "", "canary name to regenerate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--accept <name> is required")
	}
	dir := adaptDir()
	reg := contracts.NewRegistry()
	cdir := filepath.Join(dir, "contracts")
	if err := contracts.LoadDir(reg, cdir); err != nil {
		return err
	}
	canaries, err := loadCanaries(filepath.Join(dir, "canaries"))
	if err != nil {
		return err
	}
	var found *canaryCase
	for i := range canaries {
		if canaries[i].Name == *name {
			found = &canaries[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("canary %q not found (see adapt/canaries)", *name)
	}
	src := filepath.Join(dir, "fixtures", found.Fixture)
	raw, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read fixture %s: %w", found.Fixture, err)
	}
	next := nextFixtureSeq(reg, dir, found.Contract)
	nextName := fmt.Sprintf("%s.%d.json", found.Contract, next)
	dst := filepath.Join(dir, "fixtures", nextName)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("refusing to overwrite existing fixture %s", nextName)
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		return fmt.Errorf("write fixture %s: %w", nextName, err)
	}
	c, ok := reg.Get(found.Contract)
	if !ok {
		return fmt.Errorf("contract %q not registered", found.Contract)
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	rep := contracts.Diff(c, doc, kindOf(c))
	fmt.Printf("promoted %s -> %s (golden fixture for contract %s)\n", found.Fixture, nextName, found.Contract)
	fmt.Print(contracts.Summarize([]*contracts.DiffReport{rep}))
	fmt.Printf("\nnext step: add a canary case for %q with fixture %q to adapt/canaries, then re-run `mediactl adapt canary --offline`.\n", *name, nextName)
	return nil
}

// canaryCase is one golden verification unit (mirrors adapt.CanaryCase so
// the CLI can read canary files without importing the adapt harness).
type canaryCase struct {
	Name     string   `json:"name"`
	Contract string   `json:"contract"`
	Kind     string   `json:"kind"`
	Fixture  string   `json:"fixture"`
	Expect   []string `json:"expect"`
}

// loadCanaries reads every canary file under dir ({"canaries":[...]}).
func loadCanaries(dir string) ([]canaryCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []canaryCase
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var doc struct {
			Canaries []canaryCase `json:"canaries"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("canaries %s: %w", e.Name(), err)
		}
		out = append(out, doc.Canaries...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// kindOf maps a contract's category to the canary kind token.
func kindOf(c *contracts.Contract) string {
	switch c.Category {
	case "search", "items":
		return "items"
	case "comments", "replies":
		return "comments"
	case "user":
		return "users"
	case "group_members", "group":
		return "members"
	}
	return c.Category
}

// nextFixtureSeq returns the next unused <contract>.<n>.json sequence number
// by scanning the fixtures dir for the contract's existing golden files.
func nextFixtureSeq(reg *contracts.Registry, adaptDir, contractName string) int {
	c, ok := reg.Get(contractName)
	if !ok {
		return 1
	}
	prefix := c.Name + "."
	entries, err := os.ReadDir(filepath.Join(adaptDir, "fixtures"))
	if err != nil {
		return 1
	}
	max := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		rest := strings.TrimPrefix(e.Name(), prefix)
		rest = strings.TrimSuffix(rest, ".json")
		if n, err := strconv.Atoi(rest); err == nil && n > max {
			max = n
		}
	}
	return max + 1
}

// collectOptions carries the flags shared by every collect subcommand.
type collectOptions struct {
	platform  string
	signerURL string
	signerTok string
	keyword   string
	item      string
	group     string
	cid       string
	secUID    string
	mediaType string
	limit     int
	cookies   string
	account   string
	outDir    string
}

func cmdCollect(args []string) error {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "use: collect search|comments|replies|user|group|collects|collects-videos|video|im-unread (see mediactl help)\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("missing collect subcommand")
	}
	switch fs.Arg(0) {
	case "search":
		return collectSearch(fs.Args()[1:])
	case "comments":
		return collectComments(fs.Args()[1:])
	case "replies":
		return collectReplies(fs.Args()[1:])
	case "user":
		return collectUser(fs.Args()[1:])
	case "group":
		return collectGroup(fs.Args()[1:])
	case "collects":
		return collectCollects(fs.Args()[1:])
	case "collects-videos":
		return collectCollectsVideos(fs.Args()[1:])
	case "video":
		return collectVideo(fs.Args()[1:])
	case "im-unread":
		return collectIMUnread(fs.Args()[1:])
	default:
		return fmt.Errorf("unknown collect subcommand %q", fs.Arg(0))
	}
}

// collectFlagSet registers the shared collect flags (subcommand-specific
// flags are added by the caller before Parse).
func collectFlagSet(name string, o *collectOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("collect "+name, flag.ExitOnError)
	fs.StringVar(&o.platform, "platform", "", "platform: douyin|kuaishou|xhs")
	fs.StringVar(&o.cookies, "cookies", "", "cookie file; first line must be 'k1=v1; k2=v2'")
	fs.StringVar(&o.account, "account", "", "account id from the pool; the request uses its cookie/proxy/UA (pool dir: $MEDIAMON_ACCOUNTS_DIR, default data/accounts)")
	fs.StringVar(&o.outDir, "out-dir", "", "also append results to a JSONL store under this dir (collections items/comments/users/members)")
	fs.IntVar(&o.limit, "limit", 20, "max records to fetch (<=0 = no limit)")
	fs.StringVar(&o.signerURL, "signer-url", os.Getenv("MEDIAMON_SIGNER_URL"), "remote signer service base URL for signature params (default: $MEDIAMON_SIGNER_URL)")
	fs.StringVar(&o.signerTok, "signer-token", os.Getenv("MEDIAMON_SIGNER_TOKEN"), "bearer token for the signer service (default: $MEDIAMON_SIGNER_TOKEN)")
	return fs
}

// collectEngine builds the registry + engine for one collect run. The
// platform contract directories are resolved relative to the workspace adapt
// dir (MEDIAMON_ADAPT_DIR override), same as the other subcommands.
func collectEngine(o *collectOptions) (*collect.Engine, error) {
	switch o.platform {
	case douyin.Platform, kuaishou.Platform, xhs.Platform:
	default:
		return nil, fmt.Errorf("--platform must be one of douyin|kuaishou|xhs, got %q", o.platform)
	}
	cdir := filepath.Join(adaptDir(), "contracts")
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, cdir); err != nil {
		return nil, err
	}
	names := map[string]map[string]string{}
	// Each platform exposes Defaults(dir) -> (assembly, registry); the
	// engines share one registry, so only the Name maps are wired here.
	dou, _, err := douyin.Defaults(cdir)
	if err != nil {
		return nil, err
	}
	ks, _, err := kuaishou.Defaults(cdir)
	if err != nil {
		return nil, err
	}
	xh, _, err := xhs.Defaults(cdir)
	if err != nil {
		return nil, err
	}
	names[douyin.Platform] = dou.Names
	names[kuaishou.Platform] = ks.Names
	names[xhs.Platform] = xh.Names

	cookies := map[string]string{}
	if o.cookies != "" {
		hdr, err := loadCookieHeader(o.cookies)
		if err != nil {
			return nil, err
		}
		cookies[o.platform] = hdr
	}
	signers := map[string]httpclient.Signer{}
	if o.signerURL != "" {
		sc := signclient.New(signclient.Config{BaseURL: o.signerURL, Token: o.signerTok})
		for _, p := range []string{douyin.Platform, kuaishou.Platform, xhs.Platform} {
			signers[p] = sc
		}
	}
	// Account injection: --account routes every request through the account's
	// cookie/proxy/UA. Without it the engine keeps the platform defaults.
	pool, err := accountPoolFor(o.platform, o.account)
	if err != nil {
		return nil, err
	}
	eng := collect.New(collect.Context{
		Registry:  reg,
		HTTP:      sharedHTTPClient(),
		Obs:       obs.NewCounterMap(),
		Signers:   signers,
		Cookies:   cookies,
		Names:     names,
		Accounts:  pool,
		AccountID: o.account,
	})
	return eng, nil
}

// loadCookieHeader reads the platform cookie header from a file: the first
// line is "k1=v1; k2=v2". Netscape-exported files are not supported yet and
// produce a clear error.
func loadCookieHeader(file string) (string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("cookies: %w", err)
	}
	line := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	if line == "" {
		return "", fmt.Errorf("cookies: file %s is empty", file)
	}
	for _, part := range strings.Split(line, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			return "", fmt.Errorf("cookies: unsupported format in %s: put 'k1=v1; k2=v2' on the first line (Netscape export not implemented)", file)
		}
	}
	return line, nil
}

// emitRow prints one NDJSON row to stdout and optionally appends it to the
// out-dir store collection.
func emitRow(st *store.Store, collection string, rec any) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal %s row: %w", collection, err)
	}
	fmt.Println(string(b))
	if st != nil {
		if err := st.Append(collection, rec); err != nil {
			return fmt.Errorf("store append %s: %w", collection, err)
		}
	}
	return nil
}

// openOutStore opens the --out-dir store when configured.
func openOutStore(o *collectOptions) (*store.Store, error) {
	if o.outDir == "" {
		return nil, nil
	}
	st, err := store.Open(o.outDir)
	if err != nil {
		return nil, err
	}
	return st, nil
}

func collectSearch(args []string) error {
	o := &collectOptions{}
	fs := collectFlagSet("search", o)
	fs.StringVar(&o.keyword, "keyword", "", "search keyword (required)")
	fs.StringVar(&o.mediaType, "type", "", "media type filter: video|image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.keyword == "" {
		return fmt.Errorf("--keyword is required")
	}
	eng, err := collectEngine(o)
	if err != nil {
		return err
	}
	st, err := openOutStore(o)
	if err != nil {
		return err
	}
	if st != nil {
		defer st.Close()
	}
	items, _, err := eng.SearchItems(context.Background(), o.platform, o.keyword, o.mediaType, model.Cursor{}, o.limit)
	if err != nil {
		return err
	}
	for _, it := range items {
		if err := emitRow(st, "items", it); err != nil {
			return err
		}
	}
	return nil
}

func collectComments(args []string) error {
	o := &collectOptions{}
	fs := collectFlagSet("comments", o)
	fs.StringVar(&o.item, "item", "", "item id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.item == "" {
		return fmt.Errorf("--item is required")
	}
	eng, err := collectEngine(o)
	if err != nil {
		return err
	}
	st, err := openOutStore(o)
	if err != nil {
		return err
	}
	if st != nil {
		defer st.Close()
	}
	cmts, _, err := eng.ItemComments(context.Background(), o.platform, o.item, model.Cursor{}, o.limit)
	if err != nil {
		return err
	}
	for _, cm := range cmts {
		if err := emitRow(st, "comments", cm); err != nil {
			return err
		}
	}
	return nil
}

func collectReplies(args []string) error {
	o := &collectOptions{}
	fs := collectFlagSet("replies", o)
	fs.StringVar(&o.item, "item", "", "item id (required)")
	fs.StringVar(&o.cid, "cid", "", "top-level comment id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.item == "" || o.cid == "" {
		return fmt.Errorf("--item and --cid are required")
	}
	eng, err := collectEngine(o)
	if err != nil {
		return err
	}
	st, err := openOutStore(o)
	if err != nil {
		return err
	}
	if st != nil {
		defer st.Close()
	}
	// douyin and xhs declare replies contracts; a platform without one fails
	// closed with the explicit "replies contract not declared" error.
	cmts, _, err := eng.CommentReplies(context.Background(), o.platform, o.item, o.cid, model.Cursor{}, o.limit)
	if err != nil {
		return err
	}
	for _, cm := range cmts {
		if err := emitRow(st, "comments", cm); err != nil {
			return err
		}
	}
	return nil
}

func collectUser(args []string) error {
	o := &collectOptions{}
	fs := collectFlagSet("user", o)
	fs.StringVar(&o.secUID, "sec-uid", "", "user sec_uid (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.secUID == "" {
		return fmt.Errorf("--sec-uid is required")
	}
	eng, err := collectEngine(o)
	if err != nil {
		return err
	}
	st, err := openOutStore(o)
	if err != nil {
		return err
	}
	if st != nil {
		defer st.Close()
	}
	u, err := eng.UserProfile(context.Background(), o.platform, o.secUID)
	if err != nil {
		return err
	}
	return emitRow(st, "users", u)
}

func collectGroup(args []string) error {
	o := &collectOptions{}
	fs := collectFlagSet("group", o)
	fs.StringVar(&o.group, "group", "", "group id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.group == "" {
		return fmt.Errorf("--group is required")
	}
	eng, err := collectEngine(o)
	if err != nil {
		return err
	}
	st, err := openOutStore(o)
	if err != nil {
		return err
	}
	if st != nil {
		defer st.Close()
	}
	members, _, err := eng.GroupMembers(context.Background(), o.platform, o.group, model.Cursor{}, o.limit)
	if err != nil {
		return err
	}
	for _, m := range members {
		if err := emitRow(st, "members", m); err != nil {
			return err
		}
	}
	return nil
}
