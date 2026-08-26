package license

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLicenseFile drops lic as <dir>/license.json.
func writeLicenseFile(t *testing.T, dir string, lic License) {
	t.Helper()
	data, _ := json.Marshal(lic)
	if err := os.WriteFile(filepath.Join(dir, LicenseFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGateNoLicenseFile(t *testing.T) {
	pub, _, _ := GenerateKey()
	g, err := LoadGate(t.TempDir(), pub, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = g.Check("collect")
	var de *DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want *DeniedError", err)
	}
	if de.Reason != ReasonNoLicense {
		t.Fatalf("reason = %s, want %s", de.Reason, ReasonNoLicense)
	}
	if _, ok := g.Active(); ok {
		t.Fatal("gate must not report an active license")
	}
}

func TestGateValidLicensePasses(t *testing.T) {
	pub, priv, _ := GenerateKey()
	dir := t.TempDir()
	now := time.Now().Unix()
	lic := License{
		Machine:   MachineFingerprint(), // binds to the real local fingerprint
		NotBefore: now - 100,
		NotAfter:  now + 100000,
		Features:  []string{"collect", "dm"},
	}
	sig, _ := Sign(priv, lic)
	lic.Signature = sig
	writeLicenseFile(t, dir, lic)

	g, err := LoadGate(dir, pub, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Check("collect"); err != nil {
		t.Fatalf("Check collect: %v", err)
	}
	if err := g.Check(""); err != nil {
		t.Fatalf("Check any: %v", err)
	}
	var de *DeniedError
	if err := g.Check("live"); !errors.As(err, &de) || de.Reason != ReasonFeature {
		t.Fatalf("Check live: %v, want feature_disabled", err)
	}
	if got, ok := g.Active(); !ok || !got.HasFeature("dm") {
		t.Fatalf("Active = %+v, %v", got, ok)
	}
}

func TestGateExpiredDenied(t *testing.T) {
	pub, priv, _ := GenerateKey()
	v, _ := NewVerifier(pub, nil)
	v.SetMachineOverride("test-machine")
	lic := License{Machine: "test-machine", NotBefore: 100, NotAfter: 200} // long past
	sig, _ := Sign(priv, lic)
	lic.Signature = sig
	_, verr := v.Verify(lic)
	g := NewGate(v, nil, verr)
	var de *DeniedError
	if err := g.Check("collect"); !errors.As(err, &de) || de.Reason != ReasonExpired {
		t.Fatalf("err = %v, want expired_or_inactive", err)
	}
}

func TestGateMachineMismatchDenied(t *testing.T) {
	pub, priv, _ := GenerateKey()
	v, _ := NewVerifier(pub, nil)
	v.SetMachineOverride("local-machine")
	now := time.Now().Unix()
	lic := License{Machine: "other-machine", NotBefore: now - 100, NotAfter: now + 1000}
	sig, _ := Sign(priv, lic)
	lic.Signature = sig
	_, verr := v.Verify(lic)
	g := NewGate(v, nil, verr)
	var de *DeniedError
	if err := g.Check(""); !errors.As(err, &de) || de.Reason != ReasonMachine {
		t.Fatalf("err = %v, want machine_mismatch", err)
	}
}

func TestGateBadSignatureDenied(t *testing.T) {
	pub, _, _ := GenerateKey()
	dir := t.TempDir()
	lic := License{
		Machine:   "m",
		NotBefore: 0,
		NotAfter:  time.Now().Unix() + 1000,
		Signature: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",
	}
	writeLicenseFile(t, dir, lic)
	g, err := LoadGate(dir, pub, nil)
	if err != nil {
		t.Fatal(err)
	}
	var de *DeniedError
	if cerr := g.Check(""); !errors.As(cerr, &de) || de.Reason != ReasonSignature {
		t.Fatalf("err = %v, want bad_signature", cerr)
	}
}

func TestGateMalformedFileDenied(t *testing.T) {
	pub, _, _ := GenerateKey()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LicenseFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := LoadGate(dir, pub, nil)
	if err != nil {
		t.Fatal(err)
	}
	var de *DeniedError
	if cerr := g.Check(""); !errors.As(cerr, &de) || de.Reason != ReasonMalformed {
		t.Fatalf("err = %v, want malformed", cerr)
	}
}

func TestGateLoadNeverFailsOnLicenseProblems(t *testing.T) {
	// Even with garbage on disk, LoadGate returns a gate (denials are
	// structured, not load errors); a bad public key is the only load error.
	if _, err := LoadGate(t.TempDir(), nil, nil); err == nil {
		t.Fatal("expected bad-public-key load error")
	}
}
