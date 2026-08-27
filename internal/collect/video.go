// download_video atom (IR-MM-0001 AC-7 / IFACE-3): resolve + stream one
// video to disk. The bytes never ride the MCP channel (mcpio's line cap);
// callers get {path, bytes, sha256} instead.
package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DownloadResult is the download_video atom's return shape (IFACE-3).
type DownloadResult struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// DownloadVideoTo resolves one item's play URL (ResolveVideo) and streams
// the body to outDir/<platform>/<item>.mp4 (IFACE-3 layout). The write is
// atomic: a .tmp sibling absorbs the stream and renames into place on
// success, so a failure never leaves a half file. The hash is computed
// while streaming (single pass).
func (e *Engine) DownloadVideoTo(ctx context.Context, platform, itemID, outDir string) (DownloadResult, error) {
	if itemID == "" {
		return DownloadResult{}, fmt.Errorf("collect: item_id is required")
	}
	meta, err := e.ResolveVideo(ctx, platform, itemID)
	if err != nil {
		return DownloadResult{}, err
	}
	name := meta.AwemeID
	if name == "" {
		name = itemID
	}
	dir := filepath.Join(outDir, platform)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return DownloadResult{}, fmt.Errorf("collect: artifact dir: %w", err)
	}
	final := filepath.Join(dir, name+".mp4")
	tmp := final + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("collect: artifact create: %w", err)
	}
	h := sha256.New()
	n, derr := e.Download(ctx, meta.URL, io.MultiWriter(f, h))
	cerr := f.Close()
	if derr != nil {
		_ = os.Remove(tmp) // no half-file residue (AC-3)
		return DownloadResult{}, fmt.Errorf("collect: download: %w", derr)
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return DownloadResult{}, fmt.Errorf("collect: artifact close: %w", cerr)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return DownloadResult{}, fmt.Errorf("collect: artifact rename: %w", err)
	}
	return DownloadResult{Path: final, Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
