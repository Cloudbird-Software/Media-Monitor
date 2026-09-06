package accounts

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCookieHeaderDeterministic(t *testing.T) {
	a := Account{Cookies: map[string]string{"sessionid": "abc", "ttwid": "xyz", "msToken": "tok"}}
	got := a.CookieHeader()
	want := "msToken=tok; sessionid=abc; ttwid=xyz"
	if got != want {
		t.Fatalf("CookieHeader = %q, want %q", got, want)
	}
	a2 := Account{Cookies: map[string]string{"sessionid": "abc", "ttwid": "xyz", "msToken": "tok"}}
	if a2.CookieHeader() != got {
		t.Fatal("CookieHeader not deterministic across calls")
	}
}

func TestCookieHeaderEmpty(t *testing.T) {
	a := Account{}
	if a.CookieHeader() != "" {
		t.Fatalf("empty cookie header = %q", a.CookieHeader())
	}
}

func TestImportCookiesNetscape(t *testing.T) {
	input := `# Netscape HTTP Cookie File
.example.com	TRUE	/	FALSE	0	sessionid	abc123
.example.com	TRUE	/	FALSE	0	ttwid	xyz789
`
	cookies, err := ImportCookiesNetscape(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cookies["sessionid"] != "abc123" || cookies["ttwid"] != "xyz789" {
		t.Fatalf("cookies = %v", cookies)
	}
}

func TestImportCookiesNetscapeBadLine(t *testing.T) {
	_, err := ImportCookiesNetscape(strings.NewReader("too\tfew\tfields\n"))
	if err == nil || !strings.Contains(err.Error(), "bad line") {
		t.Fatalf("err = %v, want bad line error", err)
	}
}

func TestImportCookiesJSONObject(t *testing.T) {
	cookies, err := ImportCookiesJSON(strings.NewReader(`{"sessionid":"abc","ttwid":"xyz"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cookies["sessionid"] != "abc" || cookies["ttwid"] != "xyz" {
		t.Fatalf("cookies = %v", cookies)
	}
}

func TestImportCookiesJSONList(t *testing.T) {
	cookies, err := ImportCookiesJSON(strings.NewReader(`[{"name":"sessionid","value":"abc"},{"name":"ttwid","value":"xyz"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if cookies["sessionid"] != "abc" || cookies["ttwid"] != "xyz" {
		t.Fatalf("cookies = %v", cookies)
	}
}

func TestImportCookiesJSONListMissingName(t *testing.T) {
	_, err := ImportCookiesJSON(strings.NewReader(`[{"value":"abc"}]`))
	if err == nil || !strings.Contains(err.Error(), "missing name") {
		t.Fatalf("err = %v, want missing name error", err)
	}
}

func TestExportImportRoundTripNetscape(t *testing.T) {
	in := map[string]string{"sessionid": "abc", "ttwid": "xyz"}
	var buf bytes.Buffer
	if err := ExportCookiesNetscape(&buf, ".douyin.com", in); err != nil {
		t.Fatal(err)
	}
	out, err := ImportCookiesNetscape(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out["sessionid"] != "abc" || out["ttwid"] != "xyz" {
		t.Fatalf("round-trip = %v", out)
	}
}

func TestExportImportRoundTripJSON(t *testing.T) {
	in := map[string]string{"sessionid": "abc", "ttwid": "xyz"}
	var buf bytes.Buffer
	if err := ExportCookiesJSON(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ImportCookiesJSON(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out["sessionid"] != "abc" || out["ttwid"] != "xyz" {
		t.Fatalf("round-trip = %v", out)
	}
}

func TestPoolSaveListGetDelete(t *testing.T) {
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	a := Account{ID: "acc-1", Platform: "douyin", Cookies: map[string]string{"sessionid": "s1"}, Tags: []string{"vip"}}
	if err := p.Save(a); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Get("acc-1"); !ok {
		t.Fatal("acc-1 not found after Save")
	}
	if list := p.List(); len(list) != 1 || list[0].ID != "acc-1" {
		t.Fatalf("List = %+v", list)
	}
	if got, _ := p.Get("acc-1"); got.Cookies["sessionid"] != "s1" || len(got.Tags) != 1 {
		t.Fatalf("Get = %+v", got)
	}

	if err := p.Delete("acc-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Get("acc-1"); ok {
		t.Fatal("acc-1 still present after Delete")
	}
}

func TestPoolPersistenceAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Save(Account{ID: "persist-1", Platform: "kuaishou", Cookies: map[string]string{"a": "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	a, ok := p2.Get("persist-1")
	if !ok || a.Platform != "kuaishou" || a.Cookies["a"] != "b" {
		t.Fatalf("reloaded = %+v, ok=%v", a, ok)
	}
}

func TestPoolActiveFor(t *testing.T) {
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_ = p.Save(Account{ID: "a1", Platform: "douyin", Status: StatusActive})
	_ = p.Save(Account{ID: "a2", Platform: "douyin", Status: StatusPaused})
	_ = p.Save(Account{ID: "a3", Platform: "xhs", Status: StatusActive})

	active := p.ActiveFor("douyin")
	if len(active) != 1 || active[0].ID != "a1" {
		t.Fatalf("ActiveFor(douyin) = %+v", active)
	}
	if all := p.List(); len(all) != 3 {
		t.Fatalf("List = %d, want 3", len(all))
	}
}

func TestPoolSaveRequiresID(t *testing.T) {
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Save(Account{Platform: "douyin"}); err == nil {
		t.Fatal("Save should require an id")
	}
}

func TestUAPoolRotation(t *testing.T) {
	pool := NewUAPool([]string{"ua1", "ua2", "ua3"})
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		seen[pool.Next()] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected all 3 UAs seen, got %v", seen)
	}
}

func TestUAPoolSingle(t *testing.T) {
	pool := NewUAPool([]string{"only"})
	if pool.Next() != "only" || pool.Pick() != "only" {
		t.Fatal("single-UA pool should always return the only UA")
	}
}

func TestUAPoolEmptyFallsBack(t *testing.T) {
	pool := NewUAPool(nil)
	if pool.Len() != 1 || pool.Next() == "" {
		t.Fatal("empty pool should fall back to a default UA")
	}
}

func TestLoadUAPool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ua-pool.json")
	contents := `{"uas":["alpha","beta","gamma"]}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadUAPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Len() != 3 {
		t.Fatalf("Len = %d, want 3", pool.Len())
	}
}

// TestLoadUAPoolDefaultExplicit: the production loader honors an explicit
// path (used by callers that carry their own data dir).
func TestLoadUAPoolDefaultExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ua-pool.json")
	if err := os.WriteFile(path, []byte(`{"uas":["ua-one","ua-two"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadUAPoolDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Len() != 2 {
		t.Fatalf("Len = %d, want 2", pool.Len())
	}
	seen := map[string]bool{pool.Next(): true, pool.Next(): true}
	if !seen["ua-one"] && !seen["ua-two"] {
		t.Fatalf("pool yields unknown UA: %v", seen)
	}
}

// TestDefaultUAPoolPath: the default location is data/ua-pool.json relative
// to the running executable.
func TestDefaultUAPoolPath(t *testing.T) {
	p, err := DefaultUAPoolPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, filepath.Join("data", "ua-pool.json")) {
		t.Fatalf("DefaultUAPoolPath = %q, want .../data/ua-pool.json", p)
	}
}

// TestBundledUAPool: the ua-pool.json compiled into the binary (via go:embed)
// holds the silent-scraping pool of real, currently-existing desktop
// Chrome/Edge UAs (the original 44 fabricated-version Android UAs were
// replaced — report B2). This is the guaranteed-available source in CI,
// where the data/ dir is gitignored.
func TestBundledUAPool(t *testing.T) {
	pool, err := BundledUAPool()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Len() < 20 {
		t.Fatalf("bundled pool Len = %d, want >= 20 real desktop UAs", pool.Len())
	}
}
