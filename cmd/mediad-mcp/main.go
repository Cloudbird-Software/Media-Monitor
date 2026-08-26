// Command mediad-mcp serves the Media-Monitor toolset over stdio as a Model
// Context Protocol (MCP) server (newline-delimited JSON-RPC 2.0 via
// internal/mcpio). The tool list wraps the collect engine (contract registry
// from MEDIAMON_ADAPT_DIR), the live monitor session registry, the task
// runner (MEDIAMON_DATA_DIR store), the offline adaptation canary harness,
// the adb client and version reporting.
//
// Environment:
//
//	MEDIAMON_ADAPT_DIR     adapt dir (default "adapt")
//	MEDIAMON_DATA_DIR      task store dir (default "./data", relative CWD)
//	MEDIAMON_ACCOUNTS_DIR  account pool dir (default <data>/accounts)
//	MEDIAMON_UA_POOL       UA pool file (default data/ua-pool.json next to the
//	                       executable; missing file keeps the built-in pool)
//	MEDIAMON_SIGNER_URL    remote signer base URL; when unset, collect tools
//	                       for douyin fail closed (its contracts require
//	                       a_bogus) and monitor_live refuses to start unless
//	                       allow_unsigned is set
//	MEDIAMON_DOUYIN_COOKIES  "k1=v1; k2=v2" cookie header (optional)
//	MEDIAMON_KUAISHOU_COOKIES  "(optional)"
//	MEDIAMON_XHS_COOKIES   "(optional)"
//	MEDIAMON_ADB_ADDR      default adb server address (default 127.0.0.1:5037)
package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/adapt"
	"github.com/Cloudbird-Software/Media-Monitor/internal/adb"
	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/core"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/live"
	"github.com/Cloudbird-Software/Media-Monitor/internal/mcpio"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/kuaishou"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/xhs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/signclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
	"github.com/Cloudbird-Software/Media-Monitor/internal/tasks"
)

const version = "dev"

// ioReadWriter combines two independent directions into one io.ReadWriter
// (os.Stdin + os.Stdout).
type ioReadWriter struct {
	r io.Reader
	w io.Writer
}

func (x *ioReadWriter) Read(p []byte) (int, error)  { return x.r.Read(p) }
func (x *ioReadWriter) Write(p []byte) (int, error) { return x.w.Write(p) }

func main() {
	if err := run(&ioReadWriter{r: os.Stdin, w: os.Stdout}); err != nil {
		fmt.Fprintln(os.Stderr, "mediad-mcp:", err)
		os.Exit(1)
	}
}

// run wires the environment, registers every tool and serves until the
// context is canceled or the transport closes.
func run(rw io.ReadWriter) error {
	adaptDir := envOr("MEDIAMON_ADAPT_DIR", "adapt")
	dataDir := envOr("MEDIAMON_DATA_DIR", "data")
	a, err := newApp(adaptDir, dataDir)
	if err != nil {
		return err
	}
	defer a.store.Close()

	srv := mcpio.NewServer(rw)
	srv.Name = "mediad-mcp"
	srv.Version = version
	for _, t := range buildTools(a) {
		if err := srv.RegisterTool(t); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Serve(ctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// app is the dependency bundle shared by every tool handler.
type app struct {
	reg      *contracts.Registry
	engine   *collect.Engine
	baseCtx  collect.Context // base engine wiring (account_id clones it)
	runner   *core.Runner
	canary   *adapt.Runner
	sc       *signclient.Client // nil when MEDIAMON_SIGNER_URL is unset
	obs      *obs.CounterMap
	lob      *lobbyRegistry
	store    *store.Store
	accounts *accounts.Pool
}

// newApp loads the contract registry, assembles the collect engine and the
// task runner, and opens the store.
func newApp(adaptDir, dataDir string) (*app, error) {
	reg := contracts.NewRegistry()
	cdir := filepath.Join(adaptDir, "contracts")
	if err := contracts.LoadDir(reg, cdir); err != nil {
		return nil, fmt.Errorf("contracts dir %s: %w", cdir, err)
	}
	names := map[string]map[string]string{}
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

	obsMap := obs.NewCounterMap()
	signers := map[string]httpclient.Signer{}
	var sc *signclient.Client
	if u := envOr("MEDIAMON_SIGNER_URL", ""); u != "" {
		sc = signclient.New(signclient.Config{BaseURL: u}) // fail-closed by default
		for _, p := range []string{douyin.Platform, kuaishou.Platform, xhs.Platform} {
			signers[p] = sc
		}
	}
	cookies := map[string]string{}
	if v := os.Getenv("MEDIAMON_DOUYIN_COOKIES"); v != "" {
		cookies[douyin.Platform] = v
	}
	if v := os.Getenv("MEDIAMON_KUAISHOU_COOKIES"); v != "" {
		cookies[kuaishou.Platform] = v
	}
	if v := os.Getenv("MEDIAMON_XHS_COOKIES"); v != "" {
		cookies[xhs.Platform] = v
	}
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, fmt.Errorf("store %s: %w", dataDir, err)
	}
	var pool *accounts.Pool
	if p, err := accounts.Open(accountsDirEnv(dataDir)); err == nil {
		pool = p
	} else {
		fmt.Fprintf(os.Stderr, "mediad-mcp: account pool unavailable: %v (account_id routing disabled)\n", err)
	}
	baseCtx := collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{UserAgents: uaPoolUserAgents()}),
		Obs:      obsMap,
		Signers:  signers,
		Cookies:  cookies,
		Names:    names,
		Accounts: pool,
	}
	return &app{
		reg:      reg,
		engine:   collect.New(baseCtx),
		baseCtx:  baseCtx,
		runner:   core.NewRunner(st, obsMap),
		canary:   adapt.NewRunner(reg, filepath.Join(adaptDir, "fixtures"), filepath.Join(adaptDir, "canaries")),
		sc:       sc,
		obs:      obsMap,
		lob:      newLobbyRegistry(),
		store:    st,
		accounts: pool,
	}, nil
}

// engineFor returns the engine for one tool call: the shared engine for
// platform defaults, or a per-call clone pinned to a pool account (cookie/
// proxy/UA then override the platform defaults).
func (a *app) engineFor(accountID string) *collect.Engine {
	if accountID == "" || a.accounts == nil {
		return a.engine
	}
	ctx := a.baseCtx
	ctx.AccountID = accountID
	return collect.New(ctx)
}

// ---- argument coercion helpers ----

func argStr(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

func argInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func argBool(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		return err == nil && b
	case float64:
		return v != 0
	}
	return false
}

// ---- JSON Schema helpers (strict JSON Schema style) ----

func objSchema(required []string, props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func sProp(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func iProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func bProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// ---- tools ----

// buildTools returns the full tool list in registration order.
func buildTools(a *app) []mcpio.Tool {
	platformProp := sProp("platform to query: douyin|kuaishou|xhs")
	limitProp := iProp("max records to fetch (default 20)")
	accountProp := sProp("act as this account-pool id: the account's cookie/proxy/UA override the platform defaults (empty = platform default)")
	tools := []mcpio.Tool{
		{
			Name:        "search_items",
			Description: "Collect keyword search results for a platform (contract-driven).",
			InputSchema: objSchema([]string{"platform", "keyword"}, map[string]any{
				"platform":   platformProp,
				"keyword":    sProp("search keyword"),
				"media_type": sProp("media type filter: video|image"),
				"limit":      limitProp,
				"account_id": accountProp,
			}),
			Handler: a.searchItems,
		},
		{
			Name:        "get_comments",
			Description: "Collect the comment list of one item (contract-driven).",
			InputSchema: objSchema([]string{"platform", "item_id"}, map[string]any{
				"platform":   platformProp,
				"item_id":    sProp("item id"),
				"limit":      limitProp,
				"account_id": accountProp,
			}),
			Handler: a.getComments,
		},
		{
			Name:        "get_replies",
			Description: "Collect the replies of one top-level comment (contract-driven; douyin and xhs declare replies contracts, kuaishou does not — there it fails with the engine's explicit not-declared error).",
			InputSchema: objSchema([]string{"platform", "item_id", "cid"}, map[string]any{
				"platform":   platformProp,
				"item_id":    sProp("item id"),
				"cid":        sProp("top-level comment id"),
				"limit":      limitProp,
				"account_id": accountProp,
			}),
			Handler: a.getReplies,
		},
		{
			Name:        "get_user",
			Description: "Resolve one user profile by sec_uid (contract-driven).",
			InputSchema: objSchema([]string{"platform", "sec_uid"}, map[string]any{
				"platform":   platformProp,
				"sec_uid":    sProp("user sec_uid"),
				"account_id": accountProp,
			}),
			Handler: a.getUser,
		},
		{
			Name:        "group_members",
			Description: "Enumerate the members of a target group (silent enumeration, contract-driven).",
			InputSchema: objSchema([]string{"platform", "group_id"}, map[string]any{
				"platform":   platformProp,
				"group_id":   sProp("group id"),
				"limit":      limitProp,
				"account_id": accountProp,
			}),
			Handler: a.groupMembers,
		},
		{
			Name:        "resolve_video",
			Description: "Resolve one video's watermark-free play address plus cover metadata via the platform's video-download contract. Returns the address only; downloading the bytes is left to the caller (e.g. mediactl).",
			InputSchema: objSchema([]string{"platform", "item_id"}, map[string]any{
				"platform":   platformProp,
				"item_id":    sProp("item id (aweme id / note id)"),
				"account_id": accountProp,
			}),
			Handler: a.resolveVideo,
		},
		{
			Name:        "get_collects",
			Description: "List the account's bookmark folders (collects, contract-driven). Pass collects_id to list the videos inside one folder instead. Requires an account with valid cookies (account_id or the platform default cookies).",
			InputSchema: objSchema([]string{"platform"}, map[string]any{
				"platform":    platformProp,
				"collects_id": sProp("folder id: when set, list the videos inside this folder instead of the folder list"),
				"limit":       limitProp,
				"account_id":  accountProp,
			}),
			Handler: a.getCollects,
		},
		{
			Name:        "get_im_unread",
			Description: "Fetch the IM unread-message count and conversation list of an account (contract-driven). Requires an account with valid cookies (account_id or the platform default cookies).",
			InputSchema: objSchema([]string{"platform"}, map[string]any{
				"platform":   platformProp,
				"account_id": accountProp,
			}),
			Handler: a.getIMUnread,
		},
		{
			Name:        "monitor_live",
			Description: "Start a background live-room monitor session; events land in an in-memory ring buffer (200 events) for read_live_events. The platform selects the <platform>-meta contract and wire decoder (douyin protobuf, kuaishou/xhs gunzip+base64 JSON). Requires MEDIAMON_SIGNER_URL unless allow_unsigned is true (dev-only).",
			InputSchema: objSchema([]string{"room_url"}, map[string]any{
				"room_url":       sProp("live room URL, e.g. https://live.douyin.com/123456"),
				"platform":       sProp("live platform: douyin|kuaishou|xhs (default douyin)"),
				"events":         sProp("comma-separated event filter (enter,like,chat,gift,follow,fansclub,rank,seq,room_stat,control,emoji,stream); empty = all"),
				"allow_unsigned": bProp("allow starting without a signature signer (dev-only, NOT for production)"),
			}),
			Handler: a.monitorLive,
		},
		{
			Name:        "read_live_events",
			Description: "Read buffered events of a live session created by monitor_live, newest last, starting after the given sequence number.",
			InputSchema: objSchema([]string{"lobby_id"}, map[string]any{
				"lobby_id": sProp("session id returned by monitor_live"),
				"after":    iProp("return only events with seq > after (default 0 = all)"),
			}),
			Handler: a.readLiveEvents,
		},
		{
			Name:        "submit_task",
			Description: "Submit a task to the local JSONL store (queued; execution is out of scope here).",
			InputSchema: objSchema([]string{"kind"}, map[string]any{
				"kind":   sProp("task kind: search|comments|replies|users|group_members|live_monitor|flow"),
				"config": map[string]any{"type": "object", "description": "task config object (default {})"},
			}),
			Handler: a.submitTask,
		},
		{
			Name:        "list_tasks",
			Description: "List persisted tasks, newest first.",
			InputSchema: objSchema(nil, map[string]any{}),
			Handler:     a.listTasks,
		},
		{
			Name:        "adapt_canary_offline",
			Description: "Run the offline adaptation canaries against bundled fixtures and report the drift summary and health.",
			InputSchema: objSchema(nil, map[string]any{}),
			Handler:     a.adaptCanaryOffline,
		},
		{
			Name:        "contracts_list",
			Description: "List every registered platform contract name.",
			InputSchema: objSchema(nil, map[string]any{}),
			Handler:     a.contractsList,
		},
		{
			Name:        "send_message",
			Description: "Broadcast direct messages to a list of target sec_uids (contract-driven). Supports a first + optional second message with a delay, {nickname} substitution, and a per-account send cap.",
			InputSchema: objSchema([]string{"platform", "targets"}, map[string]any{
				"platform":            platformProp,
				"targets":             sProp("comma-separated or JSON array of target sec_uids"),
				"first_message":       sProp("first message text (required; use {nickname} for substitution)"),
				"second_message":      sProp("optional second message text"),
				"second_delay_ms":     iProp("delay before second message (default 15000)"),
				"send_cap":            iProp("max sends per account (0 = unlimited)"),
				"account_id":          sProp("act as this account (empty = platform default)"),
				"substitute_nickname": map[string]any{"type": "object", "description": "sec_uid -> nickname map for {nickname}"},
			}),
			Handler: a.sendMessage,
		},
		{
			Name:        "accounts_list",
			Description: "List accounts in the account pool (platform, nickname, cookie count, proxy, status).",
			InputSchema: objSchema(nil, map[string]any{
				"platform": sProp("optional platform filter: douyin|kuaishou|xhs"),
			}),
			Handler: a.accountsList,
		},
		{
			Name:        "adb_list",
			Description: "List device serials known to an adb server.",
			InputSchema: objSchema(nil, map[string]any{
				"server_addr": sProp("adb server address (default 127.0.0.1:5037 or MEDIAMON_ADB_ADDR)"),
			}),
			Handler: a.adbList,
		},
		{
			Name:        "adb_shell",
			Description: "Run a shell command on one adb device and return its combined output.",
			InputSchema: objSchema([]string{"serial", "cmd"}, map[string]any{
				"server_addr": sProp("adb server address (default 127.0.0.1:5037 or MEDIAMON_ADB_ADDR)"),
				"serial":      sProp("device serial"),
				"cmd":         sProp("shell command"),
			}),
			Handler: a.adbShell,
		},
		{
			Name:        "adb_screencap",
			Description: "Capture the screen of one adb device; returns the PNG bytes base64-encoded.",
			InputSchema: objSchema([]string{"serial"}, map[string]any{
				"server_addr": sProp("adb server address (default 127.0.0.1:5037 or MEDIAMON_ADB_ADDR)"),
				"serial":      sProp("device serial"),
			}),
			Handler: a.adbScreencap,
		},
		{
			Name:        "version",
			Description: "Report the server name and version.",
			InputSchema: objSchema(nil, map[string]any{}),
			Handler: func(_ context.Context, _ map[string]any) (any, error) {
				return map[string]any{"name": "mediad-mcp", "version": version}, nil
			},
		},
	}
	return tools
}

func validPlatform(p string) bool {
	return p == douyin.Platform || p == kuaishou.Platform || p == xhs.Platform
}

func requirePlatform(args map[string]any) (string, error) {
	p := argStr(args, "platform")
	if !validPlatform(p) {
		return "", fmt.Errorf("platform %q must be one of douyin, kuaishou, xhs", p)
	}
	return p, nil
}

func (a *app) searchItems(ctx context.Context, args map[string]any) (any, error) {
	platform, err := requirePlatform(args)
	if err != nil {
		return nil, err
	}
	keyword := argStr(args, "keyword")
	if keyword == "" {
		return nil, errors.New("keyword is required")
	}
	items, cur, err := a.engineFor(argStr(args, "account_id")).SearchItems(ctx, platform, keyword, argStr(args, "media_type"), model.Cursor{}, argInt(args, "limit", 20))
	if err != nil {
		return nil, err
	}
	return map[string]any{"items": items, "cursor": cur}, nil
}

func (a *app) getComments(ctx context.Context, args map[string]any) (any, error) {
	platform, err := requirePlatform(args)
	if err != nil {
		return nil, err
	}
	itemID := argStr(args, "item_id")
	if itemID == "" {
		return nil, errors.New("item_id is required")
	}
	cmts, cur, err := a.engineFor(argStr(args, "account_id")).ItemComments(ctx, platform, itemID, model.Cursor{}, argInt(args, "limit", 20))
	if err != nil {
		return nil, err
	}
	return map[string]any{"comments": cmts, "cursor": cur}, nil
}

func (a *app) getReplies(ctx context.Context, args map[string]any) (any, error) {
	platform, err := requirePlatform(args)
	if err != nil {
		return nil, err
	}
	itemID := argStr(args, "item_id")
	cid := argStr(args, "cid")
	if itemID == "" || cid == "" {
		return nil, errors.New("item_id and cid are required")
	}
	cmts, cur, err := a.engineFor(argStr(args, "account_id")).CommentReplies(ctx, platform, itemID, cid, model.Cursor{}, argInt(args, "limit", 20))
	if err != nil {
		return nil, err
	}
	return map[string]any{"comments": cmts, "cursor": cur}, nil
}

// resolveVideo resolves a video's watermark-free play address (M6).
func (a *app) resolveVideo(ctx context.Context, args map[string]any) (any, error) {
	platform, err := requirePlatform(args)
	if err != nil {
		return nil, err
	}
	itemID := argStr(args, "item_id")
	if itemID == "" {
		return nil, errors.New("item_id is required")
	}
	meta, err := a.engineFor(argStr(args, "account_id")).ResolveVideo(ctx, platform, itemID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"video": meta}, nil
}

// getCollects lists bookmark folders, or the videos of one folder when
// collects_id is given (M6).
func (a *app) getCollects(ctx context.Context, args map[string]any) (any, error) {
	platform, err := requirePlatform(args)
	if err != nil {
		return nil, err
	}
	eng := a.engineFor(argStr(args, "account_id"))
	limit := argInt(args, "limit", 20)
	if collectsID := argStr(args, "collects_id"); collectsID != "" {
		items, cur, err := eng.CollectVideos(ctx, platform, collectsID, model.Cursor{}, limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": items, "cursor": cur}, nil
	}
	folders, cur, err := eng.CollectFolders(ctx, platform, model.Cursor{}, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"collects": folders, "cursor": cur}, nil
}

// getIMUnread fetches the IM unread count of an account (M6).
func (a *app) getIMUnread(ctx context.Context, args map[string]any) (any, error) {
	platform, err := requirePlatform(args)
	if err != nil {
		return nil, err
	}
	res, err := a.engineFor(argStr(args, "account_id")).FetchIMUnread(ctx, platform)
	if err != nil {
		return nil, err
	}
	return map[string]any{"im_unread": res}, nil
}

func (a *app) sendMessage(ctx context.Context, args map[string]any) (any, error) {
	platform, err := requirePlatform(args)
	if err != nil {
		return nil, err
	}
	targets := splitTargets(argStr(args, "targets"))
	if len(targets) == 0 {
		return nil, errors.New("targets is required")
	}
	first := argStr(args, "first_message")
	if first == "" {
		return nil, errors.New("first_message is required")
	}
	cfg := tasks.SendTaskConfig{
		Platform:      platform,
		Targets:       targets,
		FirstMessage:  tasks.MessageTemplate{Content: first},
		SecondDelayMs: int64(argInt(args, "second_delay_ms", 15000)),
		SendCap:       argInt(args, "send_cap", 0),
		AccountID:     argStr(args, "account_id"),
	}
	if s := argStr(args, "second_message"); s != "" {
		cfg.SecondMessage = &tasks.MessageTemplate{Content: s}
	}
	if raw, ok := args["substitute_nickname"].(map[string]any); ok {
		cfg.SubstituteNick = map[string]string{}
		for k, v := range raw {
			cfg.SubstituteNick[k] = fmt.Sprint(v)
		}
	}
	rep, err := tasks.NewSender(a.engineFor(cfg.AccountID), a.store).Run(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// splitTargets accepts either a comma-separated string or a JSON array.
func splitTargets(s string) []string {
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(s), &arr); err == nil {
			return arr
		}
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (a *app) getUser(ctx context.Context, args map[string]any) (any, error) {
	platform, err := requirePlatform(args)
	if err != nil {
		return nil, err
	}
	secUID := argStr(args, "sec_uid")
	if secUID == "" {
		return nil, errors.New("sec_uid is required")
	}
	u, err := a.engineFor(argStr(args, "account_id")).UserProfile(ctx, platform, secUID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"user": u}, nil
}

func (a *app) groupMembers(ctx context.Context, args map[string]any) (any, error) {
	platform, err := requirePlatform(args)
	if err != nil {
		return nil, err
	}
	groupID := argStr(args, "group_id")
	if groupID == "" {
		return nil, errors.New("group_id is required")
	}
	members, cur, err := a.engineFor(argStr(args, "account_id")).GroupMembers(ctx, platform, groupID, model.Cursor{}, argInt(args, "limit", 20))
	if err != nil {
		return nil, err
	}
	return map[string]any{"members": members, "cursor": cur}, nil
}

// ---- live monitor sessions ----

// defaultRingCapacity bounds the per-session event buffer.
const defaultRingCapacity = 200

// ringEntry is one buffered event with its session sequence number.
type ringEntry struct {
	Seq   int             `json:"seq"`
	Event model.LiveEvent `json:"event"`
}

// eventRing is a thread-safe fixed-capacity buffer of live events with
// monotonic sequence numbers; overflow drops the oldest events.
type eventRing struct {
	mu      sync.Mutex
	cap     int
	buf     []ringEntry
	start   int
	count   int
	nextSeq int
	roomID  string
	ended   bool
	endErr  string
}

func newEventRing(capacity int) *eventRing {
	return &eventRing{cap: capacity}
}

func (r *eventRing) append(ev model.LiveEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := ringEntry{Seq: r.nextSeq, Event: ev}
	r.nextSeq++
	if r.count < r.cap {
		r.buf = append(r.buf, entry)
		r.count++
	} else {
		r.buf[r.start] = entry
		r.start = (r.start + 1) % r.cap
	}
	if ev.RoomID != "" {
		r.roomID = ev.RoomID
	}
}

func (r *eventRing) end(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = true
	if err != nil {
		r.endErr = err.Error()
	}
}

// since returns every buffered entry with Seq > after, oldest first.
func (r *eventRing) since(after int) []ringEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ringEntry
	for i := 0; i < r.count; i++ {
		e := r.buf[(r.start+i)%r.cap]
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out
}

func (r *eventRing) snapshot() (roomID string, ended bool, endErr string, nextSeq int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.roomID, r.ended, r.endErr, r.nextSeq
}

// liveLobby is one background monitoring session.
type liveLobby struct {
	id      string
	roomURL string
	ring    *eventRing
}

// lobbyRegistry tracks every live session for the process lifetime.
type lobbyRegistry struct {
	mu       sync.Mutex
	sessions map[string]*liveLobby
}

func newLobbyRegistry() *lobbyRegistry {
	return &lobbyRegistry{sessions: map[string]*liveLobby{}}
}

func (l *lobbyRegistry) register(roomURL string) *liveLobby {
	lob := &liveLobby{id: newLobbyID(), roomURL: roomURL, ring: newEventRing(defaultRingCapacity)}
	l.mu.Lock()
	l.sessions[lob.id] = lob
	l.mu.Unlock()
	return lob
}

func (l *lobbyRegistry) get(id string) (*liveLobby, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lob, ok := l.sessions[id]
	return lob, ok
}

func newLobbyID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("lobby-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("lobby-%s", hex.EncodeToString(b[:]))
}

func (a *app) monitorLive(_ context.Context, args map[string]any) (any, error) {
	roomURL := argStr(args, "room_url")
	if roomURL == "" {
		return nil, errors.New("room_url is required")
	}
	platform := argStr(args, "platform")
	if platform == "" {
		platform = douyin.Platform
	}
	if !validPlatform(platform) {
		return nil, fmt.Errorf("platform %q must be one of douyin, kuaishou, xhs", platform)
	}
	allowUnsigned := argBool(args, "allow_unsigned")
	var signer live.SignFn
	switch {
	case a.sc != nil:
		signer = a.sc.WSSSignatureSigner(platform + "-live")
	case allowUnsigned:
		// Dev-only deterministic stub; NOT a real signature. Production must
		// always configure MEDIAMON_SIGNER_URL (see docs/HARDENING.md).
		signer = md5StubSigner
	default:
		return nil, errors.New("no signature signer configured: set MEDIAMON_SIGNER_URL, or pass allow_unsigned=true for dev-only unsigned monitoring")
	}
	var filter map[string]bool
	if events := argStr(args, "events"); events != "" {
		filter = map[string]bool{}
		for _, e := range strings.Split(events, ",") {
			if e = strings.TrimSpace(e); e != "" {
				filter[e] = true
			}
		}
	}
	cfg := &live.Config{
		HTTP:     httpclient.New(httpclient.Config{UserAgents: uaPoolUserAgents()}),
		Registry: a.reg,
		Signer:   signer,
		Obs:      a.obs,
		Platform: platform,
		Decoder:  liveDecoderFor(a.reg, platform),
	}
	lob := a.lob.register(roomURL)
	go func() {
		// The session runs on its own context: it must outlive the tool call
		// and ends when the stream ends, fails, or the process exits.
		err := cfg.Connect(context.Background(), roomURL, func(ev model.LiveEvent) error {
			if filter != nil && !filter[ev.Event] {
				return nil
			}
			lob.ring.append(ev)
			a.obs.Inc("mcp.live_event", 1)
			return nil
		})
		if err != nil {
			a.obs.Inc("mcp.live_error", 1)
		}
		lob.ring.end(err)
	}()
	roomID, _, _, _ := lob.ring.snapshot()
	return map[string]any{"session_id": lob.id, "room_id": roomID}, nil
}

func (a *app) readLiveEvents(_ context.Context, args map[string]any) (any, error) {
	id := argStr(args, "lobby_id")
	if id == "" {
		id = argStr(args, "session_id")
	}
	if id == "" {
		return nil, errors.New("lobby_id is required")
	}
	after := argInt(args, "after", 0)
	if after < 0 {
		return nil, errors.New("after must be >= 0")
	}
	lob, ok := a.lob.get(id)
	if !ok {
		return nil, fmt.Errorf("no live session with lobby_id %q: sessions live in memory for the process lifetime; use monitor_live first", id)
	}
	evs := lob.ring.since(after)
	next := after
	if len(evs) > 0 {
		next = evs[len(evs)-1].Seq
	}
	roomID, ended, endErr, _ := lob.ring.snapshot()
	res := map[string]any{
		"session_id": id,
		"room_id":    roomID,
		"events":     evs,
		"next":       next,
		"ended":      ended,
	}
	if endErr != "" {
		res["end_error"] = endErr
	}
	return res, nil
}

// liveDecoderFor selects the platform wire decoder from the platform's
// <platform>-meta contract: kuaishou/xhs speak gunzip+base64 JSON frames,
// douyin uses the built-in protobuf path (nil decoder).
func liveDecoderFor(reg *contracts.Registry, platform string) live.Decoder {
	var metaName string
	switch platform {
	case kuaishou.Platform:
		metaName = "kuaishou-meta"
	case xhs.Platform:
		metaName = "xhs-meta"
	default:
		return nil // douyin protobuf path
	}
	c, ok := reg.Get(metaName)
	if !ok || c == nil || len(c.ProtoMethods) == 0 {
		return nil
	}
	if platform == kuaishou.Platform {
		return live.NewKuaishouDecoder(c.ProtoMethods)
	}
	return live.NewXhsDecoder(c.ProtoMethods)
}

// md5StubSigner is an explicitly non-production signature placeholder for
// local development (same stub as cmd/mediactl live monitor).
func md5StubSigner(urlQuery string, params map[string]string) (string, error) {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(params[k])
	}
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

// ---- tasks, canary, contracts, adb ----

func (a *app) submitTask(_ context.Context, args map[string]any) (any, error) {
	kind := argStr(args, "kind")
	if kind == "" {
		return nil, errors.New("kind is required")
	}
	cfg := model.JSONMap{}
	if raw, ok := args["config"]; ok && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("config must be a JSON object")
		}
		cfg = m
	}
	task, err := a.runner.Submit(kind, cfg)
	if err != nil {
		return nil, err
	}
	return task, nil
}

func (a *app) listTasks(_ context.Context, _ map[string]any) (any, error) {
	tasks, err := a.runner.List()
	if err != nil {
		return nil, err
	}
	if tasks == nil {
		tasks = []model.Task{}
	}
	return map[string]any{"tasks": tasks}, nil
}

func (a *app) adaptCanaryOffline(_ context.Context, _ map[string]any) (any, error) {
	reports, err := a.canary.RunAllOffline()
	if err != nil {
		return nil, err
	}
	healthy := true
	for _, r := range reports {
		if !r.Healthy() {
			healthy = false
		}
	}
	return map[string]any{
		"report":  contracts.Summarize(reports),
		"healthy": healthy,
		"cases":   len(reports),
	}, nil
}

func (a *app) contractsList(_ context.Context, _ map[string]any) (any, error) {
	return map[string]any{"contracts": a.reg.List()}, nil
}

func (a *app) accountsList(_ context.Context, args map[string]any) (any, error) {
	if a.accounts == nil {
		return map[string]any{"accounts": []accounts.Account{}}, nil
	}
	platform := argStr(args, "platform")
	out := []accounts.Account{}
	for _, acct := range a.accounts.List() {
		if platform != "" && acct.Platform != platform {
			continue
		}
		out = append(out, acct)
	}
	return map[string]any{"accounts": out}, nil
}

// adbServerAddr resolves the adb server address: explicit arg beats
// MEDIAMON_ADB_ADDR, which beats 127.0.0.1:5037.
func adbServerAddr(args map[string]any) string {
	if v := argStr(args, "server_addr"); v != "" {
		return v
	}
	return envOr("MEDIAMON_ADB_ADDR", "127.0.0.1:5037")
}

func (a *app) adbList(_ context.Context, args map[string]any) (any, error) {
	addr := adbServerAddr(args)
	var c adb.Client // ListDevices dials its own fresh connection
	devs, err := c.ListDevices(addr)
	if err != nil {
		return nil, err
	}
	return map[string]any{"server_addr": addr, "devices": devs}, nil
}

func (a *app) adbShell(_ context.Context, args map[string]any) (any, error) {
	serial := argStr(args, "serial")
	cmd := argStr(args, "cmd")
	if serial == "" || cmd == "" {
		return nil, errors.New("serial and cmd are required")
	}
	c, err := adb.Connect(adbServerAddr(args))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	out, err := c.Shell(serial, cmd)
	if err != nil {
		return nil, err
	}
	return map[string]any{"output": out}, nil
}

func (a *app) adbScreencap(_ context.Context, args map[string]any) (any, error) {
	serial := argStr(args, "serial")
	if serial == "" {
		return nil, errors.New("serial is required")
	}
	c, err := adb.Connect(adbServerAddr(args))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	png, err := c.ScreencapPNG(serial)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"png_base64": base64.StdEncoding.EncodeToString(png),
		"bytes":      len(png),
	}, nil
}
