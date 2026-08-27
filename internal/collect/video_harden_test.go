package collect

// video_harden_test.go — path-injection hardening for the restored
// download_video atom (CodeQL go/path-injection remediation on top of the
// byte-identical W3-C3 recovery). Verifies that request-derived segments can
// never alter the artifacts layout: traversal ids, separators, dot-only names
// and control bytes all fail closed before any filesystem write.

import (
	"strings"
	"testing"
)

func TestSafeSegmentRejectsLayoutAlteringNames(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"7660000000000000001", true}, // normal aweme id (VR slice artifact)
		{"abc-DEF_123.x", true},       // charset whitelist forms
		{"", false},                   // empty
		{".", false}, {"..", false},   // dot-only traversal forms
		{"../evil", false},                // unix traversal
		{"..\\evil", false},               // windows traversal
		{"a/b", false},                    // separator injection
		{"a\\b", false},                   // windows separator injection
		{"id\nline", false},               // control byte
		{"id\x00nul", false},              // NUL byte
		{strings.Repeat("a", 201), false}, // oversize
	}
	for _, c := range cases {
		if got := safeSegment(c.in); got != c.want {
			t.Errorf("safeSegment(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// DownloadVideoTo must fail closed on an unsafe platform segment BEFORE any
// network resolution or filesystem write happens.
func TestDownloadVideoToFailsClosedOnUnsafePlatform(t *testing.T) {
	e := New(Context{}) // empty wiring is fine: the guard fires pre-network
	for _, platform := range []string{"../escape", "a/b", "..", ""} {
		res, err := e.DownloadVideoTo(t.Context(), platform, "some-id", t.TempDir())
		if err == nil {
			t.Errorf("platform %q accepted; want fail-closed error", platform)
			continue
		}
		if !strings.Contains(err.Error(), "invalid platform segment") {
			t.Errorf("platform %q: unexpected error %v", platform, err)
		}
		if res.Path != "" || res.Bytes != 0 {
			t.Errorf("platform %q: partial result %v returned on failure", platform, res)
		}
	}
}
