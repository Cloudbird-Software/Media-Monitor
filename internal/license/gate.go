package license

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LicenseFileName is the license file name inside the data directory.
const LicenseFileName = "license.json"

// DeniedReason classifies why the gate refused an operation. It is stable
// vocabulary for the cmd layer to map onto "refuse collection/actions, keep
// the dashboard and version available".
type DeniedReason string

const (
	ReasonNoLicense DeniedReason = "no_license"          // no license file present
	ReasonMalformed DeniedReason = "malformed"           // file unreadable/undecodable
	ReasonSignature DeniedReason = "bad_signature"       // Ed25519 verification failed
	ReasonMachine   DeniedReason = "machine_mismatch"    // bound to another machine
	ReasonExpired   DeniedReason = "expired_or_inactive" // outside the time window
	ReasonOnline    DeniedReason = "online_failed"       // online check failed
	ReasonFeature   DeniedReason = "feature_disabled"    // valid license, feature off
)

// DeniedError is the structured refusal returned by Gate.Check.
type DeniedError struct {
	Reason DeniedReason `json:"reason"`
	Detail string       `json:"detail,omitempty"`
}

func (e *DeniedError) Error() string {
	if e.Detail == "" {
		return "license: denied: " + string(e.Reason)
	}
	return fmt.Sprintf("license: denied: %s (%s)", e.Reason, e.Detail)
}

// DenialFields extracts the transport-independent refusal fields from a Gate
// denial for the surfaces that render one (HTTP 403 body, MCP tool error):
// reason is the stable DeniedReason vocabulary, detail the human context. An
// error that is not a *DeniedError yields ("unknown", err.Error()) so every
// surface renders a fail-closed refusal instead of dropping it.
func DenialFields(err error) (reason, detail string) {
	reason, detail = "unknown", err.Error()
	var de *DeniedError
	if errors.As(err, &de) {
		reason = string(de.Reason)
		if de.Detail != "" {
			detail = de.Detail
		}
	}
	return reason, detail
}

// DenialMessage renders a Gate denial as a structured JSON object message,
// {"error":"license_denied","reason":...,"detail":...}, for tool-style error
// channels whose payload is a plain error string.
func DenialMessage(err error) error {
	reason, detail := DenialFields(err)
	raw, merr := json.Marshal(map[string]any{
		"error":  "license_denied",
		"reason": reason,
		"detail": detail,
	})
	if merr != nil {
		return fmt.Errorf("license_denied: %s: %s", reason, detail)
	}
	return errors.New(string(raw))
}

// Gate is the cmd-layer license enforcer: it loads the license file from the
// data directory once, verifies it, and answers Check(feature) for every
// gated operation. Fail-closed: any problem yields a *DeniedError. The
// dashboard/version surfaces must NOT consult the gate.
type Gate struct {
	v        *Verifier
	lic      *License // verified active license, nil when denied
	deny     *DeniedError
	filePath string
}

// NewGate builds a Gate from parts. lic must be non-nil and verifyErr nil for
// gated operations to pass; otherwise Check denies with the classified reason.
// Tests use it to inject a verifier with an overridden clock/machine.
func NewGate(v *Verifier, lic *License, verifyErr error) *Gate {
	g := &Gate{v: v}
	if verifyErr != nil {
		g.deny = &DeniedError{Reason: classify(verifyErr), Detail: verifyErr.Error()}
		return g
	}
	if lic == nil {
		g.deny = &DeniedError{Reason: ReasonNoLicense}
		return g
	}
	l := *lic
	g.lic = &l
	return g
}

// LoadGate loads <dataDir>/license.json, verifies it (Ed25519 signature,
// machine fingerprint, time window, optional online check) and returns the
// gate. LoadGate itself never fails on license problems — they surface as
// structured denials from Check. It errors only when the verifier cannot be
// constructed (bad public key).
func LoadGate(dataDir string, pub ed25519.PublicKey, online OnlineVerifier) (*Gate, error) {
	v, err := NewVerifier(pub, online)
	if err != nil {
		return nil, err
	}
	g := &Gate{v: v, filePath: filepath.Join(dataDir, LicenseFileName)}
	data, err := os.ReadFile(g.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			g.deny = &DeniedError{Reason: ReasonNoLicense, Detail: g.filePath + " not found"}
		} else {
			g.deny = &DeniedError{Reason: ReasonMalformed, Detail: err.Error()}
		}
		return g, nil
	}
	var lic License
	if err := json.Unmarshal(data, &lic); err != nil {
		g.deny = &DeniedError{Reason: ReasonMalformed, Detail: err.Error()}
		return g, nil
	}
	verified, verr := v.Verify(lic)
	if verr != nil {
		g.deny = &DeniedError{Reason: classify(verr), Detail: verr.Error()}
		return g, nil
	}
	g.lic = &verified
	return g, nil
}

// classify maps a Verify error onto a DeniedReason.
func classify(err error) DeniedReason {
	switch {
	case errors.Is(err, ErrInvalidSignature):
		return ReasonSignature
	case errors.Is(err, ErrNotActive):
		return ReasonExpired
	case errors.Is(err, ErrMachineMismatch):
		return ReasonMachine
	case errors.Is(err, ErrOnlineFailed):
		return ReasonOnline
	default:
		return ReasonMalformed
	}
}

// Check reports whether a gated operation (collection, send, trace, ...) is
// allowed. feature may be "" (any valid license suffices) or a feature flag
// from License.Features. A nil return means allowed; otherwise the error is
// a *DeniedError whose Reason the caller maps to its refusal behavior.
func (g *Gate) Check(feature string) error {
	if g.deny != nil {
		return g.deny
	}
	if g.lic == nil {
		return &DeniedError{Reason: ReasonNoLicense}
	}
	if feature != "" && !g.lic.HasFeature(feature) {
		return &DeniedError{Reason: ReasonFeature, Detail: feature}
	}
	return nil
}

// Active returns the verified license, or false when the gate is denying.
func (g *Gate) Active() (License, bool) {
	if g.lic == nil {
		return License{}, false
	}
	return *g.lic, true
}
