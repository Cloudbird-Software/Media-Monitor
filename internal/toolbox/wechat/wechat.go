// Package wechat implements the WeChat multi-open tool of the original
// software (views/utils/mWechat.vue → controller.utils.weixin {num}): the
// front end asks for N instances and the main process spawns N copies of the
// bundled helper openwechat.exe (shipped under extraResources/more/). This Go
// port keeps the behavior local and testable: the helper path is configurable,
// a missing helper fails closed, and process launching is abstracted behind
// an injectable Launcher.
package wechat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// DefaultHelperRelPath is the helper location relative to the running
// executable, matching the original packaging layout.
const DefaultHelperRelPath = "extraResources/more/openwechat.exe"

// Launcher starts one helper instance. It must return after the process is
// started (not wait for exit). Tests inject a fake.
type Launcher func(path string) error

// Config tunes Launch.
type Config struct {
	// HelperPath is the openwechat.exe path. Empty = DefaultHelperRelPath
	// resolved against the executable's directory.
	HelperPath string
	// Launcher starts each instance. Nil = exec.Command(path).Start().
	Launcher Launcher
}

// DefaultHelperPath resolves the bundled helper path against the executable.
func DefaultHelperPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("wechat: locate executable: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), filepath.FromSlash(DefaultHelperRelPath)), nil
}

// Launch starts num instances of the helper. Fail-closed: num < 1 and a
// missing helper executable are errors, and if any instance fails to start
// the error is returned immediately (already-started instances are left
// running, mirroring the original fire-and-forget behavior).
func Launch(cfg Config, num int) error {
	if num < 1 {
		return fmt.Errorf("wechat: num must be >= 1, got %d", num)
	}
	path := cfg.HelperPath
	if path == "" {
		var err error
		path, err = DefaultHelperPath()
		if err != nil {
			return err
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("wechat: helper not found at %s (fail-closed): %w", path, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("wechat: helper path %s is a directory", path)
	}
	launch := cfg.Launcher
	if launch == nil {
		launch = func(p string) error {
			cmd := exec.Command(p)
			cmd.Dir = filepath.Dir(p)
			return cmd.Start()
		}
	}
	for i := 0; i < num; i++ {
		if err := launch(path); err != nil {
			return fmt.Errorf("wechat: start instance %d/%d: %w", i+1, num, err)
		}
	}
	return nil
}
