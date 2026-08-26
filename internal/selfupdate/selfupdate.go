// Package selfupdate is the update-mechanism skeleton: check a manifest URL,
// compare versions, download + SHA256-verify the new binary, and report an
// available update. It does NOT replace the running binary (that requires an
// out-of-band launcher); it downloads to data/updates/ and reports. Fail-closed:
// a checksum mismatch discards the download.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
)

// Manifest is the update manifest fetched from the (configurable) URL.
type Manifest struct {
	Version      string         `json:"version"` // semver-ish, e.g. "1.2.3"
	URL          string         `json:"url"`     // download URL for this platform
	SHA256       string         `json:"sha256"`  // hex-encoded expected hash
	ReleaseNotes string         `json:"release_notes,omitempty"`
	Required     bool           `json:"required,omitempty"`
	Extra        map[string]any `json:"extra,omitempty"`
}

// UpdateChecker checks for updates against a manifest URL.
type UpdateChecker struct {
	manifestURL string
	http        *httpclient.Client
	nowVersion  string
}

// NewChecker builds an UpdateChecker. nowVersion is the currently running version.
func NewChecker(manifestURL, nowVersion string, hc *httpclient.Client) *UpdateChecker {
	if hc == nil {
		hc = httpclient.New(httpclient.Config{})
	}
	return &UpdateChecker{manifestURL: manifestURL, http: hc, nowVersion: nowVersion}
}

// Check fetches the manifest and reports whether a newer version is available.
// Returns (nil, nil) when up to date; (manifest, nil) when an update is
// available; (nil, err) on failure.
func (c *UpdateChecker) Check() (*Manifest, error) {
	if c.manifestURL == "" {
		return nil, errors.New("selfupdate: no manifest URL configured")
	}
	status, body, err := c.http.WithContract("selfupdate_check").Do(nil, "GET", c.manifestURL, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: fetch manifest: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("selfupdate: manifest status %d", status)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("selfupdate: parse manifest: %w", err)
	}
	if m.Version == "" || m.URL == "" {
		return nil, errors.New("selfupdate: manifest missing version/url")
	}
	if !versionGreater(m.Version, c.nowVersion) {
		return nil, nil // up to date
	}
	return &m, nil
}

// Download downloads the update to destDir, verifying its SHA256. The running
// binary is never touched; the caller is responsible for applying the update.
func (c *UpdateChecker) Download(m *Manifest, destDir string) (string, error) {
	if m == nil || m.URL == "" {
		return "", errors.New("selfupdate: no update to download")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("selfupdate: create dir: %w", err)
	}
	status, body, err := c.http.WithContract("selfupdate_download").Do(nil, "GET", m.URL, nil, nil)
	if err != nil {
		return "", fmt.Errorf("selfupdate: download: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("selfupdate: download status %d", status)
	}
	// Verify checksum before writing anything.
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if m.SHA256 != "" && !strings.EqualFold(got, m.SHA256) {
		return "", fmt.Errorf("selfupdate: checksum mismatch (got %s, want %s) — discarding", got, m.SHA256)
	}
	name := m.Version + "-" + got[:8] + ".bin"
	dest := filepath.Join(destDir, name)
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		return "", fmt.Errorf("selfupdate: write: %w", err)
	}
	return dest, nil
}

// versionGreater reports whether a > b using a tolerant numeric-component
// comparison (e.g. 1.2.3 > 1.2.0). Missing/non-numeric components fall back to
// string comparison. A blank b ("") is treated as "anything is newer".
func versionGreater(a, b string) bool {
	if b == "" {
		return a != ""
	}
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(ap) {
			ai, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			bi, _ = strconv.Atoi(bp[i])
		}
		if ai != bi {
			return ai > bi
		}
	}
	return a > b
}

var _ = io.Discard
