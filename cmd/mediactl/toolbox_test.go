package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolboxEmbedExtractRoundtrip(t *testing.T) {
	run := func(args ...string) (string, error) {
		return captureStdout(t, func() error { return cmdToolbox(args) })
	}
	first, err := run("encrypt", "embed", "--text", "hello 世界", "--secret", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	embedded := strings.TrimSpace(first)
	if embedded == "hello 世界" {
		t.Fatal("embed did not alter the text")
	}
	// Same secret => deterministic pattern.
	second, err := run("encrypt", "embed", "--text", "hello 世界", "--secret", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("same secret produced different output:\n%q\n%q", first, second)
	}
	restored, err := run("encrypt", "extract", "--text", embedded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(restored) != "hello 世界" {
		t.Fatalf("extract = %q", restored)
	}
}

func TestToolboxEmbedMinMaxValidation(t *testing.T) {
	err := cmdToolbox([]string{"encrypt", "embed", "--text", "x", "--min", "30", "--max", "10"})
	if err == nil || !strings.Contains(err.Error(), "--min") {
		t.Fatalf("error = %v", err)
	}
	if err := cmdToolbox([]string{"encrypt", "nope"}); err == nil {
		t.Fatal("unknown encrypt subcommand accepted")
	}
	if err := cmdToolbox([]string{"nope"}); err == nil {
		t.Fatal("unknown toolbox subcommand accepted")
	}
}

func TestToolboxStylizeSingleAndFile(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return cmdToolbox([]string{"stylize", "--phone", "13800138000", "--style"})
	})
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(out)
	if line == "" || line == "13800138000" {
		t.Fatalf("stylize output = %q", line)
	}
	for _, r := range line {
		if r >= '0' && r <= '9' {
			t.Fatalf("ASCII digit survived stylize: %q", line)
		}
	}

	file := filepath.Join(t.TempDir(), "phones.txt")
	if err := os.WriteFile(file, []byte("13800138000\n13900139000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = captureStdout(t, func() error {
		return cmdToolbox([]string{"stylize", "--phones-file", file, "--separator"})
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("stylize lines = %q", out)
	}

	if err := cmdToolbox([]string{"stylize", "--phone", "1", "--phones-file", file}); err == nil {
		t.Fatal("--phone and --phones-file together accepted")
	}
}

func TestToolboxWechatMulti(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "openwechat.exe")
	if err := os.WriteFile(helper, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	launched := 0
	old := wechatLauncher
	wechatLauncher = func(path string) error {
		launched++
		if path != helper {
			t.Errorf("launcher path = %q, want %q", path, helper)
		}
		return nil
	}
	defer func() { wechatLauncher = old }()

	out, err := captureStdout(t, func() error {
		return cmdToolbox([]string{"wechat-multi", "--num", "3", "--helper-path", helper})
	})
	if err != nil {
		t.Fatal(err)
	}
	if launched != 3 {
		t.Fatalf("launched = %d, want 3", launched)
	}
	if !strings.Contains(out, "launched 3 wechat instance(s)") {
		t.Fatalf("output = %q", out)
	}

	if err := cmdToolbox([]string{"wechat-multi", "--num", "0", "--helper-path", helper}); err == nil {
		t.Fatal("--num 0 accepted")
	}
	// Missing helper fails closed (library behavior, wired through).
	if err := cmdToolbox([]string{"wechat-multi", "--num", "1", "--helper-path", filepath.Join(t.TempDir(), "none.exe")}); err == nil {
		t.Fatal("missing helper accepted")
	}
}
