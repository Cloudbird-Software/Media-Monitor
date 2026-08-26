package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// collect_m6.go — the M6 collection surfaces: watermark-free video resolve /
// download, bookmark (collects) folders and their videos, and the IM unread
// count. All are contract-driven via the collect engine and share the
// account-pool / license-gate wiring of the other collect subcommands.

// parseAwemeID extracts the item id from a video page URL (the last path
// segment) or returns a bare id unchanged.
func parseAwemeID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("--url or --aweme-id is required")
	}
	if !strings.Contains(raw, "/") {
		return raw, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse --url: %w", err)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	id := strings.TrimSpace(segs[len(segs)-1])
	if id == "" {
		return "", fmt.Errorf("no item id found in --url %q", raw)
	}
	return id, nil
}

// collectVideo resolves one item's watermark-free play address (and cover)
// through the platform's video-download contract. --download streams the
// video to --out-dir/<aweme_id>.mp4.
func collectVideo(args []string) error {
	o := &collectOptions{}
	fs := collectFlagSet("video", o)
	rawURL := fs.String("url", "", "video page URL; the item id is taken from the last path segment")
	awemeID := fs.String("aweme-id", "", "item id directly (takes precedence over --url)")
	download := fs.Bool("download", false, "stream the resolved video to --out-dir/<aweme_id>.mp4")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := *awemeID
	if id == "" {
		var err error
		id, err = parseAwemeID(*rawURL)
		if err != nil {
			return err
		}
	}
	if *download && o.outDir == "" {
		return fmt.Errorf("--download requires --out-dir")
	}
	eng, err := collectEngine(o)
	if err != nil {
		return err
	}
	ctx := context.Background()
	meta, err := eng.ResolveVideo(ctx, o.platform, id)
	if err != nil {
		return err
	}
	if *download {
		if err := os.MkdirAll(o.outDir, 0o755); err != nil {
			return err
		}
		dst := filepath.Join(o.outDir, meta.AwemeID+".mp4")
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		n, derr := eng.Download(ctx, meta.URL, f)
		cerr := f.Close()
		if derr != nil {
			return fmt.Errorf("download %s: %w", dst, derr)
		}
		if cerr != nil {
			return cerr
		}
		meta.Bytes = n
		fmt.Fprintf(os.Stderr, "downloaded %s (%d bytes)\n", dst, n)
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// collectCollects lists the account's bookmark/collects folders (paginated).
func collectCollects(args []string) error {
	o := &collectOptions{}
	fs := collectFlagSet("collects", o)
	if err := fs.Parse(args); err != nil {
		return err
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
	items, _, err := eng.CollectFolders(context.Background(), o.platform, model.Cursor{}, o.limit)
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

// collectCollectsVideos lists the videos inside one collects folder
// (paginated).
func collectCollectsVideos(args []string) error {
	o := &collectOptions{}
	fs := collectFlagSet("collects-videos", o)
	fs.StringVar(&o.item, "folder-id", "", "collects folder id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.item == "" {
		return fmt.Errorf("--folder-id is required")
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
	items, _, err := eng.CollectVideos(context.Background(), o.platform, o.item, model.Cursor{}, o.limit)
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

// collectIMUnread prints the IM unread count and the conversation summary as
// one JSON row.
func collectIMUnread(args []string) error {
	o := &collectOptions{}
	fs := collectFlagSet("im-unread", o)
	if err := fs.Parse(args); err != nil {
		return err
	}
	eng, err := collectEngine(o)
	if err != nil {
		return err
	}
	res, err := eng.FetchIMUnread(context.Background(), o.platform)
	if err != nil {
		return err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
