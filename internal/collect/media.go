// download_media atom (IR-MM-0002 AC-2 / IFACE-6): persist one note's image
// set to artifacts/<platform>/<item>/NNN.<ext> plus a manifest.json. Every
// URL must hit the platform's image-CDN allowlist (fail-closed
// cdn_host_not_allowed before any byte moves); writes are atomic
// (tmp+rename) and the manifest rides per-file sha256 so a consumer (VR's
// OCR stage) can verify what it reads.
package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// cdnAllowlist: public image-CDN host suffixes per platform. Tests override
// this via the cdnAllowlistOverride knob (host suffixes never cover internal
// naming in production).
var cdnAllowlist = map[string][]string{
	"douyin":   {"douyinpic.com", "zjcdn.com"},
	"kuaishou": {"kwimgs.com", "kscdn.com"},
	"xhs":      {"xhscdn.com"},
}

// cdnAllowlistOverride replaces the production allowlist when non-nil
// (test-only; never set by shipped wiring).
var cdnAllowlistOverride map[string][]string

// ImageFile is one downloaded image in the note-images manifest.
type ImageFile struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// MediaManifest is the download_media note_images return shape (IFACE-6).
type MediaManifest struct {
	Platform     string      `json:"platform"`
	ItemID       string      `json:"item_id"`
	Files        []ImageFile `json:"files"`
	TotalBytes   int64       `json:"total_bytes"`
	ManifestPath string      `json:"manifest_path"`
}

// hostAllowed reports whether host ends at an allowlisted suffix on a dot
// boundary (attacker-xhscdn.com must not pass as xhscdn.com).
func hostAllowed(suffixes []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, s := range suffixes {
		s = strings.ToLower(s)
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

var mediaImageExt = map[string]bool{
	".webp": true, ".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".gif": true,
}

// DownloadNoteImages streams each URL to outDir/<platform>/<itemID>/NNN.<ext>
// and writes manifest.json last (the manifest's presence is the completion
// marker: a failed run never leaves one behind). The bytes are plain CDN GETs
// — no signing, no cookies — so the only trust boundary is the allowlist.
func (e *Engine) DownloadNoteImages(ctx context.Context, platform, itemID string, urls []string, outDir string) (MediaManifest, error) {
	if itemID == "" || !itemIDPattern.MatchString(itemID) {
		return MediaManifest{}, fmt.Errorf("collect: item_id %q is not a safe artifact key", itemID)
	}
	if !safeSegment(platform) {
		return MediaManifest{}, fmt.Errorf("collect: invalid platform segment %q", platform)
	}
	if len(urls) == 0 {
		return MediaManifest{}, fmt.Errorf("collect: note_images requires urls (from the user-posts listing atom's extra.images)")
	}
	allow := cdnAllowlist[platform]
	if cdnAllowlistOverride != nil {
		allow = cdnAllowlistOverride[platform]
	}
	if len(allow) == 0 {
		return MediaManifest{}, fmt.Errorf("collect: no image CDN allowlist declared for platform %q", platform)
	}
	dir := filepath.Join(filepath.Clean(outDir), platform, itemID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return MediaManifest{}, fmt.Errorf("collect: artifact dir: %w", err)
	}
	manifest := MediaManifest{Platform: platform, ItemID: itemID, Files: make([]ImageFile, 0, len(urls))}
	for i, raw := range urls {
		u, perr := url.Parse(strings.TrimSpace(raw))
		if perr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return MediaManifest{}, fmt.Errorf("collect: image url %d is not an absolute http(s) url", i+1)
		}
		if !hostAllowed(allow, u.Hostname()) {
			return MediaManifest{}, fmt.Errorf("collect: image url %d host %q is not on the %s image CDN allowlist: cdn_host_not_allowed", i+1, u.Host, platform)
		}
		ext := strings.ToLower(path.Ext(u.Path))
		if !mediaImageExt[ext] {
			ext = ".img"
		}
		name := fmt.Sprintf("%02d%s", i+1, ext)
		final := filepath.Join(dir, name)
		tmp := final + ".tmp"
		f, cerr := os.Create(tmp)
		if cerr != nil {
			return MediaManifest{}, fmt.Errorf("collect: artifact create: %w", cerr)
		}
		h := sha256.New()
		n, derr := e.Download(ctx, raw, io.MultiWriter(f, h))
		ferr := f.Close()
		if derr != nil || ferr != nil {
			_ = os.Remove(tmp)
			return MediaManifest{}, fmt.Errorf("collect: image %d download: %w", i+1, derr)
		}
		if rerr := os.Rename(tmp, final); rerr != nil {
			_ = os.Remove(tmp)
			return MediaManifest{}, fmt.Errorf("collect: artifact rename: %w", rerr)
		}
		manifest.Files = append(manifest.Files, ImageFile{
			URL: raw, Path: final, Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil)),
		})
		manifest.TotalBytes += n
	}
	mb, merr := json.MarshalIndent(manifest, "", "  ")
	if merr != nil {
		return MediaManifest{}, fmt.Errorf("collect: manifest marshal: %w", merr)
	}
	mp := filepath.Join(dir, "manifest.json")
	if werr := atomicWrite(mp, mb); werr != nil {
		return MediaManifest{}, fmt.Errorf("collect: manifest write: %w", werr)
	}
	manifest.ManifestPath = mp
	return manifest, nil
}

// atomicWrite writes data to path via a tmp sibling + rename so a reader
// never observes a half manifest.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		if werr != nil {
			return werr
		}
		return cerr
	}
	return os.Rename(tmp, path)
}
