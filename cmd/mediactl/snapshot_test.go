package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSnapshotAdaptDir creates a minimal temp adapt dir with one contract,
// one golden fixture (sequence 1) and one canary referencing it.
func writeSnapshotAdaptDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"contracts", "fixtures", "canaries"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	contract := `{"name":"c1","platform":"douyin","category":"search","version":"1",
		"transport":{"base_url":"https://example.com","path":"/search","method":"GET"},
		"binding":{"items":"$.items"}}`
	if err := os.WriteFile(filepath.Join(dir, "contracts", "c1.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture := `{"items":[{"aweme_id":"a1"}]}`
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "c1.1.json"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	canary := `{"canaries":[{"name":"case1","contract":"c1","kind":"items","fixture":"c1.1.json","expect":[]}]}`
	if err := os.WriteFile(filepath.Join(dir, "canaries", "c.json"), []byte(canary), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAdaptSnapshotAccept: --accept promotes the canary's fixture to the next
// golden sequence and keeps the old fixture.
func TestAdaptSnapshotAccept(t *testing.T) {
	dir := writeSnapshotAdaptDir(t)
	t.Setenv("MEDIAMON_ADAPT_DIR", dir)

	out, err := captureStdout(t, func() error {
		return adaptSnapshot([]string{"--accept", "case1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "promoted c1.1.json -> c1.2.json") {
		t.Fatalf("output = %q", out)
	}
	// New sequence fixture exists with the same payload; the old one is kept.
	newRaw, err := os.ReadFile(filepath.Join(dir, "fixtures", "c1.2.json"))
	if err != nil {
		t.Fatal(err)
	}
	oldRaw, err := os.ReadFile(filepath.Join(dir, "fixtures", "c1.1.json"))
	if err != nil {
		t.Fatal("old fixture must be kept")
	}
	if string(newRaw) != string(oldRaw) {
		t.Fatalf("promoted fixture differs: %q vs %q", newRaw, oldRaw)
	}

	// A second accept advances the sequence again (c1.3.json), never
	// overwriting an existing fixture.
	if _, err := captureStdout(t, func() error {
		return adaptSnapshot([]string{"--accept", "case1"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fixtures", "c1.3.json")); err != nil {
		t.Fatalf("second accept did not create c1.3.json: %v", err)
	}

	// Unknown canary fails.
	if err := adaptSnapshot([]string{"--accept", "nope"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown canary error = %v", err)
	}
}
