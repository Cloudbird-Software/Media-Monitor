package main

import "os"

// Test-binary default: disable inter-page pacing so tests that exercise the
// collect engine do not really sleep between pages.
func init() {
	_ = os.Setenv("MEDIAMON_PAGE_SLEEP_MS", "0")
}
