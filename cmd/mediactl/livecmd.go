package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/live"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/signclient"
)

// defaultLiveEvents is the full event vocabulary of the live monitor.
var defaultLiveEvents = []string{
	"enter", "like", "chat", "gift", "follow", "fansclub", "rank",
	"seq", "room_stat", "control", "emoji", "stream",
}

// cmdLive dispatches the live monitor subcommands.
func cmdLive(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use: live monitor --room <url> [flags]")
	}
	switch args[0] {
	case "monitor":
		return liveMonitor(args[1:])
	default:
		return fmt.Errorf("unknown live subcommand %q", args[0])
	}
}

// liveMonitor monitors one room until stream end / signals and prints
// NDJSON events (optionally persisted to a store).
func liveMonitor(args []string) error {
	fs := flag.NewFlagSet("live monitor", flag.ExitOnError)
	room := fs.String("room", "", "live room URL, e.g. https://live.douyin.com/123456")
	events := fs.String("events", strings.Join(defaultLiveEvents, ","), "comma-separated event filter (enter,like,chat,gift,follow,fansclub,rank,seq,room_stat,control,emoji,stream)")
	signerURL := fs.String("signer-url", os.Getenv("MEDIAMON_SIGNER_URL"), "remote signer service base URL (default: $MEDIAMON_SIGNER_URL)")
	allowUnsigned := fs.Bool("allow-unsigned", false, "allow dialing without a signature signer (production must not use this)")
	reconnect := fs.Int("reconnect", 3, "max reconnect attempts")
	outDir := fs.String("out-dir", "", "also append events to a JSONL store under this dir (collection live_events)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *room == "" {
		return fmt.Errorf("--room is required")
	}
	filter := map[string]bool{}
	for _, e := range strings.Split(*events, ",") {
		e = strings.TrimSpace(e)
		if e != "" {
			filter[e] = true
		}
	}

	cdir := filepathAdaptContracts()
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, cdir); err != nil {
		return err
	}

	// Pick the platform decoder from the live room URL host (kuaishou/xhs use
	// the gunzip+base64 JSON decoders; douyin uses the built-in protobuf path).
	decoder := liveDecoderFor(reg, *room)

	var signer live.SignFn
	switch {
	case *signerURL != "":
		sc := signclient.New(signclient.Config{BaseURL: *signerURL, ReturnUnsigned: false})
		signer = sc.WSSSignatureSigner("douyin-live")
	case *allowUnsigned:
		// Dev-only deterministic stub: NOT a real signature. Producing
		// builds must always use the remote signer service.
		signer = live.MD5StubSigner
	default:
		return fmt.Errorf("no signature signer configured: set --signer-url/$MEDIAMON_SIGNER_URL, or pass --allow-unsigned for local development")
	}

	st, err := openOutStore(&collectOptions{outDir: *outDir})
	if err != nil {
		return err
	}
	if st != nil {
		defer st.Close()
	}

	cfg := &live.Config{
		HTTP:         sharedHTTPClient(),
		Registry:     reg,
		Signer:       signer,
		Obs:          obs.NewCounterMap(),
		ReconnectMax: *reconnect,
		Decoder:      decoder,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	handler := func(ev model.LiveEvent) error {
		if len(filter) > 0 && !filter[ev.Event] {
			return nil
		}
		return emitRow(st, "live_events", ev)
	}
	return cfg.Connect(ctx, *room, handler)
}

// filepathAdaptContracts resolves the adapt contracts dir for the live cmd.
func filepathAdaptContracts() string {
	dir := adaptDir()
	return dir + string(os.PathSeparator) + "contracts"
}

// liveDecoderFor selects a platform decoder from the room URL host. douyin
// uses the built-in protobuf path (nil); kuaishou/xhs use gunzip+base64 JSON
// decoders built from their live_meta contract's protocol_methods.
func liveDecoderFor(reg *contracts.Registry, roomURL string) live.Decoder {
	if roomURL == "" {
		return nil
	}
	u := roomURL
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	pu, err := url.Parse(u)
	if err != nil {
		return nil
	}
	host := strings.ToLower(pu.Hostname())
	meta := "douyin-meta"
	switch host {
	case "live.kuaishou.com", "www.live.kuaishou.com":
		meta = "kuaishou-meta"
	case "www.xiaohongshu.com", "xiaohongshu.com":
		meta = "xhs-meta"
	}
	c, ok := reg.Get(meta)
	if !ok || c == nil || len(c.ProtoMethods) == 0 {
		// Fall back to douyin protobuf for unknown/undeclared platforms.
		if d, ok := reg.Get("douyin-meta"); ok && d != nil {
			return nil // douyin path
		}
		return nil
	}
	switch meta {
	case "kuaishou-meta":
		return live.NewKuaishouDecoder(c.ProtoMethods)
	case "xhs-meta":
		return live.NewXhsDecoder(c.ProtoMethods)
	}
	return nil
}
