package waterlevel

import (
	"path/filepath"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
)

// fakeNotifier records issue-plane actions.
type fakeNotifier struct {
	open   map[string]int // platform -> issue number (0 = closed)
	next   int
	opened []Alert
	closed []int
	notes  []string
}

func (f *fakeNotifier) OpenIssue(a Alert) (int, error) {
	f.next++
	f.open[a.Platform] = f.next
	f.opened = append(f.opened, a)
	return f.next, nil
}
func (f *fakeNotifier) OpenIssueNumber(platform string) (int, error) {
	return f.open[platform], nil
}
func (f *fakeNotifier) CloseIssue(num int, comment string) error {
	for p, n := range f.open {
		if n == num {
			delete(f.open, p)
		}
	}
	f.closed = append(f.closed, num)
	f.notes = append(f.notes, comment)
	return nil
}

func newFake() *fakeNotifier { return &fakeNotifier{open: map[string]int{}} }

func poolWith(t *testing.T, rows ...accounts.Account) *accounts.Pool {
	t.Helper()
	p, err := accounts.Open(filepath.Join(t.TempDir(), "pool"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	for _, a := range rows {
		if err := p.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// TestWaterlevelOpensBelowThreshold: one usable account under threshold 2
// opens a drift issue with all five body elements (AC-1/AC-5).
func TestWaterlevelOpensBelowThreshold(t *testing.T) {
	pool := poolWith(t,
		accounts.Account{ID: "d1", Platform: "douyin"},
		accounts.Account{ID: "d2", Platform: "douyin", Status: accounts.StatusBanned},
		accounts.Account{ID: "d3", Platform: "douyin", Cookies: map[string]string{"ttwid": "secret-fragment"}},
	)
	_ = pool.SetHealth("d1", accounts.HealthHealthy, "")
	_ = pool.SetHealth("d3", accounts.HealthExpired, "")
	n := newFake()
	opened, _, err := Run(pool, n, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || len(n.opened) != 1 {
		t.Fatalf("opened = %v", opened)
	}
	a := n.opened[0]
	if a.Platform != "douyin" || a.Usable != 1 || a.Threshold != 2 {
		t.Fatalf("alert = %+v", a)
	}
	body := a.IssueBody()
	for _, want := range []string{"douyin", "1", "2", "d1", "ENV-REQ-2", "#12", "accounts import"} {
		if !contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if contains(body, "secret-fragment") || contains(body, "ttwid") {
		t.Fatal("body leaks credential fragments (AC-5 masking)")
	}
}

// TestWaterlevelIdempotent: an already-open platform issue is not duplicated
// (AC-2).
func TestWaterlevelIdempotent(t *testing.T) {
	pool := poolWith(t, accounts.Account{ID: "d1", Platform: "douyin"})
	n := newFake()
	if _, _, err := Run(pool, n, 2); err != nil {
		t.Fatal(err)
	}
	opened, _, err := Run(pool, n, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 0 || len(n.opened) != 1 {
		t.Fatalf("second run opened %v (want idempotent)", opened)
	}
}

// TestWaterlevelRecovers: climbing back over the threshold closes the open
// issue with a recovery comment (AC-3).
func TestWaterlevelRecovers(t *testing.T) {
	pool := poolWith(t, accounts.Account{ID: "d1", Platform: "douyin"})
	n := newFake()
	if _, _, err := Run(pool, n, 2); err != nil {
		t.Fatal(err)
	}
	if err := pool.Save(accounts.Account{ID: "d2", Platform: "douyin"}); err != nil {
		t.Fatal(err)
	}
	opened, closed, err := Run(pool, n, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 0 || len(closed) != 1 || len(n.notes) != 1 {
		t.Fatalf("recovery: opened=%v closed=%v notes=%v", opened, closed, n.notes)
	}
}

// TestWaterlevelThresholdEnv: the threshold is caller-configurable (the
// daemon maps MEDIAMON_WATERLEVEL_MIN onto it; AC-4).
func TestWaterlevelThresholdEnv(t *testing.T) {
	pool := poolWith(t,
		accounts.Account{ID: "d1", Platform: "douyin"},
		accounts.Account{ID: "d2", Platform: "douyin"},
	)
	n := newFake()
	if _, _, err := Run(pool, n, 3); err != nil {
		t.Fatal(err)
	}
	if len(n.opened) != 1 {
		t.Fatal("threshold 3 with 2 usable must alert")
	}
	n2 := newFake()
	if _, _, err := Run(pool, n2, 2); err != nil {
		t.Fatal(err)
	}
	if len(n2.opened) != 0 {
		t.Fatal("threshold 2 with 2 usable must not alert")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
