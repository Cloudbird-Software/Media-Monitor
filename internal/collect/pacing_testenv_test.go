package collect

import "os"

// Test-binary default: disable inter-page pacing so existing pagination tests
// do not really sleep (dedicated pacing tests build explicit Context.Pacing
// configs and are unaffected by this env override).
func init() {
	_ = os.Setenv("MEDIAMON_PAGE_SLEEP_MS", "0")
}
