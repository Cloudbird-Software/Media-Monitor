// Contract-driven collection test helpers shared by the platform and engine
// tests: they resolve the repository adapt/contracts tree offline and
// re-register named contracts against an httptest server so the real
// contract fixtures drive the flows with zero external network.

package testkit

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
)

// ContractsDir resolves the repository adapt/contracts dir from a test
// package located `levels` directories below the repo root (e.g. a platform
// package internal/platforms/<p> uses 3, internal/collect uses 2).
func ContractsDir(t *testing.T, levels int) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < levels; i++ {
		dir = filepath.Join(dir, "..")
	}
	return filepath.Join(dir, "adapt", "contracts")
}

// RemapContracts loads every contract in dir into a throwaway registry and
// returns a new registry holding only names, with each copy's transport base
// URL pointed at srv (usually an httptest.Server). Missing contracts fail
// the test.
func RemapContracts(t *testing.T, dir string, srv *httptest.Server, names ...string) *contracts.Registry {
	t.Helper()
	all := contracts.NewRegistry()
	if err := contracts.LoadDir(all, dir); err != nil {
		t.Fatal(err)
	}
	reg := contracts.NewRegistry()
	for _, n := range names {
		c, ok := all.Get(n)
		if !ok {
			t.Fatalf("contract %q not found in %s", n, dir)
		}
		cp := *c
		cp.Transport.BaseURL = srv.URL
		if err := reg.Add(&cp); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}
