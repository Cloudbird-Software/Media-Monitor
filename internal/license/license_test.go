package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(pub, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Override machine + clock for the test.
	v.now = func() int64 { return 1000 }
	lic := License{Machine: "test-machine", NotBefore: 500, NotAfter: 2000, Features: []string{"collect", "dm"}}
	sig, err := Sign(priv, lic)
	if err != nil {
		t.Fatal(err)
	}
	lic.Signature = sig

	v.SetMachineOverride("test-machine")
	got, err := v.Verify(lic)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !got.IsActive(1000) {
		t.Fatal("license should be active")
	}
	if !got.HasFeature("collect") {
		t.Fatal("missing feature")
	}
}

func TestExpiredLicense(t *testing.T) {
	pub, priv, _ := GenerateKey()
	v, _ := NewVerifier(pub, nil)
	v.now = func() int64 { return 3000 } // past NotAfter
	lic := License{Machine: "test-machine", NotBefore: 500, NotAfter: 2000, Signature: "x"}
	sig, _ := Sign(priv, lic)
	lic.Signature = sig
	if _, err := v.Verify(lic); err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestMachineMismatch(t *testing.T) {
	pub, priv, _ := GenerateKey()
	v, _ := NewVerifier(pub, nil)
	v.now = func() int64 { return 1000 }
	lic := License{Machine: "different-machine", NotBefore: 500, NotAfter: 2000}
	sig, _ := Sign(priv, lic)
	lic.Signature = sig
	v.SetMachineOverride("test-machine")
	if _, err := v.Verify(lic); err == nil || !strings.Contains(err.Error(), "machine mismatch") {
		t.Fatalf("expected machine mismatch, got %v", err)
	}
}

func TestTamperedSignature(t *testing.T) {
	pub, _, _ := GenerateKey()
	v, _ := NewVerifier(pub, nil)
	v.now = func() int64 { return 1000 }
	lic := License{Machine: "test-machine", NotBefore: 500, NotAfter: 2000, Signature: base64.StdEncoding.EncodeToString(make([]byte, 64))}
	if _, err := v.Verify(lic); err == nil || !strings.Contains(err.Error(), "invalid signature") {
		t.Fatalf("expected signature error, got %v", err)
	}
}

func TestBadPublicKeyLength(t *testing.T) {
	if _, err := NewVerifier(ed25519.PublicKey([]byte("short")), nil); err == nil {
		t.Fatal("expected bad key length error")
	}
}

func TestOnlineVerifier(t *testing.T) {
	pub, priv, _ := GenerateKey()
	online := &stubOnline{ok: false}
	v, _ := NewVerifier(pub, online)
	v.now = func() int64 { return 1000 }
	v.SetMachineOverride("test-machine")
	lic := License{Machine: "test-machine", NotBefore: 500, NotAfter: 2000}
	sig, _ := Sign(priv, lic)
	lic.Signature = sig
	if _, err := v.Verify(lic); err == nil {
		t.Fatal("expected online verification failure")
	}
	online.ok = true
	if _, err := v.Verify(lic); err != nil {
		t.Fatalf("online OK should pass: %v", err)
	}
}

type stubOnline struct {
	ok  bool
	err error
}

func (s *stubOnline) Verify(machine string, lic License) (bool, error) {
	return s.ok, s.err
}

var _ = time.Now
