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
	"regexp"
	"strings"
)

// itemIDPattern constrains caller-supplied item ids to plain identifier
// characters (digits / ASCII letters / - _ .) of bounded length, so a
// crafted id can never contribute a separator or traversal form.
var itemIDPattern = regexp.MustCompile(`^[0-9A-Za-z._-]{1,64}$`)

// DownloadResult is the download_video atom's return shape (IFACE-3).
type DownloadResult struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// safeSegment reports whether s may stand alone as one path segment:
// non-empty, bounded length, no separators, no dot-only forms and no
// control bytes. Request-derived components (platform / item id) are
// validated here so a crafted id can never alter the artifacts layout
// outside outDir/<platform>/<name>.mp4 (fail closed).
func safeSegment(s string) bool {
	if s == "" || len(s) > 200 || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, `/\`) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
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
	if !itemIDPattern.MatchString(itemID) {
		return DownloadResult{}, fmt.Errorf("collect: item_id %q is not a safe artifact key", itemID)
	}
	if !safeSegment(platform) {
		return DownloadResult{}, fmt.Errorf("collect: invalid platform segment %q", platform)
	}
	meta, err := e.ResolveVideo(ctx, platform, itemID)
	if err != nil {
		return DownloadResult{}, err
	}
	name := meta.AwemeID
	if name == "" {
		name = itemID
	}
	if !safeSegment(name) {
		return DownloadResult{}, fmt.Errorf("collect: item id %q is not a safe artifact name", name)
	}
	root := filepath.Clean(outDir)
	dir := filepath.Join(root, platform)
	final := filepath.Join(dir, name+".mp4")
	tmp := final + ".tmp"
	// Containment post-condition: resolve `final` relative to the cleaned
	// root and refuse any result that climbs out of it (`..` segments). A
	// clean relative walk from root to the artifact proves the layout stays
	// inside the artifacts tree; anything else is a fail-closed error.
	absRoot, aerr := filepath.Abs(root)
	if aerr != nil {
		return DownloadResult{}, fmt.Errorf("collect: artifacts root: %w", aerr)
	}
	absFinal, ferr := filepath.Abs(final)
	if ferr != nil {
		return DownloadResult{}, fmt.Errorf("collect: artifact path: %w", ferr)
	}
	rel, rerr := filepath.Rel(absRoot, absFinal)
	if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return DownloadResult{}, fmt.Errorf("collect: artifact path escapes artifacts root: %s", final)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return DownloadResult{}, fmt.Errorf("collect: artifact dir: %w", err)
	}
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
