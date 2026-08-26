// Package license is the client-side license/activation/device-binding core.
// It never issues licenses (the signing server is out of scope — see
// docs/LICENSE-PROTOCOL.md); it only verifies them, fail-closed:
//   - Machine fingerprint: Windows machine GUID (read from the registry via
//     os/exec → reg query / wmic, stdlib-only — no golang.org/x/sys).
//   - Offline verification: Ed25519 signature over (machine || expiry || features),
//     verified with a bundled Ed25519 public key (stdlib crypto/ed25519).
//   - Online verification (optional): an interface the host wires to its license
//     server; when unset, only offline checks apply.
//
// When no valid license is active, collection/action interfaces must refuse
// service (the dashboard and version remain available). This package exposes
// the gate; the HTTP/MCP/REST layers consult it.
package license

import (
	"crypto/ed25519"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrNoLicense is returned when no valid license is active.
var ErrNoLicense = errors.New("license: no valid license")

// Typed verification failures, wrapped by Verify so callers (e.g. Gate) can
// classify with errors.Is. Messages keep their human-readable detail.
var (
	ErrInvalidSignature = errors.New("license: invalid signature")
	ErrNotActive        = errors.New("license: not active")
	ErrMachineMismatch  = errors.New("license: machine mismatch")
	ErrOnlineFailed     = errors.New("license: online verification failed")
)

// License is one verified license document.
type License struct {
	Machine    string   `json:"machine"`    // bound machine fingerprint
	NotBefore  int64    `json:"not_before"` // unix seconds
	NotAfter   int64    `json:"not_after"`  // unix seconds (expiry)
	Features   []string `json:"features"`   // enabled feature flags
	Issuer     string   `json:"issuer,omitempty"`
	Signature  string   `json:"signature"` // base64 Ed25519 signature over the payload
	RawPayload []byte   `json:"-"`         // canonical signed payload (set by Verify)
}

// IsActive reports whether the license is valid at now (signature already
// verified, machine matched, and within its time window).
func (l License) IsActive(now int64) bool {
	return l.Signature != "" && l.Machine != "" && now >= l.NotBefore && now <= l.NotAfter
}

// HasFeature reports whether the named feature is enabled.
func (l License) HasFeature(f string) bool {
	for _, ft := range l.Features {
		if ft == f {
			return true
		}
	}
	return false
}

// Verifier verifies licenses against a public key and the local machine.
type Verifier struct {
	pub     ed25519.PublicKey
	online  OnlineVerifier
	now     func() int64
	machine func() string // override for tests; nil = MachineFingerprint
}

// OnlineVerifier is the optional online-check surface. The host wires its
// license server here; when nil, only offline verification applies.
type OnlineVerifier interface {
	Verify(machine string, lic License) (bool, error)
}

// NewVerifier builds a Verifier. pub must be a 32-byte Ed25519 public key.
func NewVerifier(pub ed25519.PublicKey, online OnlineVerifier) (*Verifier, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("license: bad public key length %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	return &Verifier{pub: pub, online: online, now: func() int64 { return time.Now().Unix() }}, nil
}

// MachineFingerprint returns the Windows machine GUID (stdlib-only: reg query
// falls back to wmic, then to a hashed default). Deterministic per machine.
func MachineFingerprint() string {
	g := readMachineGUID()
	if g == "" {
		g = "unknown-" + hostnameHash()
	}
	return g
}

// readMachineGUID reads HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid via
// `reg query` (stdlib). Falls back to wmic if reg is unavailable.
func readMachineGUID() string {
	if out, err := exec.Command("reg", "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "MachineGuid") {
				parts := strings.Fields(line)
				if len(parts) >= 3 {
					return strings.TrimSpace(parts[2])
				}
			}
		}
	}
	if out, err := exec.Command("wmic", "csproduct", "get", "UUID").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "UUID") {
				return line
			}
		}
	}
	return ""
}

func hostnameHash() string {
	h, _ := exec.Command("hostname").Output()
	sum := sha1.Sum([]byte(strings.TrimSpace(string(h))))
	return hexEncode(sum[:])
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}

// payloadBytes is the canonical signed content (stable field order).
func payloadBytes(l License) ([]byte, error) {
	p := struct {
		Machine   string   `json:"machine"`
		NotBefore int64    `json:"not_before"`
		NotAfter  int64    `json:"not_after"`
		Features  []string `json:"features"`
		Issuer    string   `json:"issuer,omitempty"`
	}{
		Machine: l.Machine, NotBefore: l.NotBefore, NotAfter: l.NotAfter,
		Features: l.Features, Issuer: l.Issuer,
	}
	return json.Marshal(p)
}

// Verify checks a license: signature over the payload, machine match, time
// window, and (if wired) the online check. On success the verified License
// (with RawPayload set) is returned.
func (v *Verifier) Verify(lic License) (License, error) {
	payload, err := payloadBytes(lic)
	if err != nil {
		return License{}, fmt.Errorf("license: marshal payload: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(lic.Signature)
	if err != nil {
		return License{}, fmt.Errorf("license: decode signature: %w", err)
	}
	if !ed25519.Verify(v.pub, payload, sig) {
		return License{}, ErrInvalidSignature
	}
	lic.RawPayload = payload
	now := v.now()
	if !lic.IsActive(now) {
		return License{}, fmt.Errorf("%w (now=%d window=[%d,%d])", ErrNotActive, now, lic.NotBefore, lic.NotAfter)
	}
	localMachine := MachineFingerprint()
	if v.machine != nil {
		localMachine = v.machine()
	}
	if lic.Machine != localMachine {
		return License{}, fmt.Errorf("%w (license=%q local=%q)", ErrMachineMismatch, lic.Machine, localMachine)
	}
	if v.online != nil {
		ok, oerr := v.online.Verify(lic.Machine, lic)
		if oerr != nil || !ok {
			return License{}, fmt.Errorf("%w: %v", ErrOnlineFailed, oerr)
		}
	}
	return lic, nil
}

// Sign is a helper for the (out-of-scope) issuer: it signs a license payload
// with an Ed25519 private key, returning the base64 signature. Kept here so
// the signing algorithm is documented alongside verification.
func Sign(priv ed25519.PrivateKey, lic License) (string, error) {
	payload, err := payloadBytes(lic)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload)), nil
}

// GenerateKey creates a new Ed25519 keypair (for the issuer / tests).
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(nil)
}

// SetMachineOverride overrides the machine fingerprint (tests only).
func (v *Verifier) SetMachineOverride(m string) {
	v.machine = func() string { return m }
}

var _ = strconv.Itoa
