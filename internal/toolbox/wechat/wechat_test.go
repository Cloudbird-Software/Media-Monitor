package wechat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeHelper creates an executable-shaped file to stand in for
// openwechat.exe.
func writeFakeHelper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openwechat.exe")
	if err := os.WriteFile(path, []byte("MZ-fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLaunchStartsNInstances: Launch must invoke the launcher exactly num
// times, each time with the configured helper path.
func TestLaunchStartsNInstances(t *testing.T) {
	helper := writeFakeHelper(t)
	var calls []string
	cfg := Config{
		HelperPath: helper,
		Launcher: func(path string) error {
			calls = append(calls, path)
			return nil
		},
	}
	if err := Launch(cfg, 3); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("launcher calls = %d, want 3", len(calls))
	}
	for i, p := range calls {
		if p != helper {
			t.Fatalf("call %d path = %q, want %q", i, p, helper)
		}
	}
}

// TestLaunchMissingHelperFailsClosed: a nonexistent helper must be an error
// before any launch attempt.
func TestLaunchMissingHelperFailsClosed(t *testing.T) {
	launched := 0
	cfg := Config{
		HelperPath: filepath.Join(t.TempDir(), "no-such.exe"),
		Launcher: func(string) error {
			launched++
			return nil
		},
	}
	err := Launch(cfg, 2)
	if err == nil || !strings.Contains(err.Error(), "helper not found") {
		t.Fatalf("expected fail-closed missing-helper error, got %v", err)
	}
	if launched != 0 {
		t.Fatalf("no instance may start without the helper, got %d", launched)
	}
}

// TestLaunchRejectsBadNum: num < 1 is an error and starts nothing.
func TestLaunchRejectsBadNum(t *testing.T) {
	helper := writeFakeHelper(t)
	launched := 0
	cfg := Config{HelperPath: helper, Launcher: func(string) error { launched++; return nil }}
	for _, n := range []int{0, -1} {
		if err := Launch(cfg, n); err == nil {
			t.Fatalf("num=%d must fail", n)
		}
	}
	if launched != 0 {
		t.Fatalf("launched = %d, want 0", launched)
	}
}

// TestLaunchStopsOnFirstFailure: a launcher error aborts the remaining
// instances and is surfaced.
func TestLaunchStopsOnFirstFailure(t *testing.T) {
	helper := writeFakeHelper(t)
	boom := errors.New("spawn refused")
	launched := 0
	cfg := Config{
		HelperPath: helper,
		Launcher: func(string) error {
			launched++
			if launched == 2 {
				return boom
			}
			return nil
		},
	}
	err := Launch(cfg, 5)
	if err == nil || !strings.Contains(err.Error(), "2/5") {
		t.Fatalf("expected instance-indexed error, got %v", err)
	}
	if launched != 2 {
		t.Fatalf("launched = %d, want 2 (stopped at first failure)", launched)
	}
}

// TestLaunchHelperIsDirectory: a directory at the helper path fails closed.
func TestLaunchHelperIsDirectory(t *testing.T) {
	cfg := Config{HelperPath: t.TempDir(), Launcher: func(string) error { return nil }}
	if err := Launch(cfg, 1); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory error, got %v", err)
	}
}
