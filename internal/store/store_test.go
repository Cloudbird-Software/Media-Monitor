package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

// TestAppendScanRoundTrip: 500 random JSONMap records round-trip exactly and
// Stats reports the row count.
func TestAppendScanRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	rng := testkit.NewR(4242)
	collect := "events"
	recs := make([]model.JSONMap, 0, 500)
	for i := 0; i < 500; i++ {
		rec := rng.Map(3, 5, 4)
		recs = append(recs, rec)
		if err := st.Append(collect, rec); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	var got []model.JSONMap
	if err := st.Scan(collect, func(raw []byte) error {
		var m model.JSONMap
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("bad row: %v (raw=%s)", err, raw)
		}
		got = append(got, m)
		return nil
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 500 {
		t.Fatalf("scanned %d rows, want 500", len(got))
	}
	for i := range recs {
		want, _ := json.Marshal(recs[i])
		have, _ := json.Marshal(got[i])
		if !bytes.Equal(want, have) {
			t.Fatalf("row #%d diverged:\nwant %s\nhave %s", i, want, have)
		}
	}

	stats := st.Stats()
	if stats[collect] != 500 {
		t.Fatalf("Stats[%q] = %d, want 500", collect, stats[collect])
	}
}

// TestOpenCreatesDir: Open on a missing directory creates it.
func TestOpenCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if err := st.Append("x", model.JSONMap{"v": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// TestScanMissingCollection: scanning a collection that was never written is
// a no-op, not an error.
func TestScanMissingCollection(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	calls := 0
	if err := st.Scan("nope", func(raw []byte) error { calls++; return nil }); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if calls != 0 {
		t.Fatalf("got %d callbacks for missing collection", calls)
	}
}

// TestAppendInvalidCollectionName: names that could escape the directory are
// rejected, and names are isolated per collection.
func TestAppendInvalidCollectionName(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	for _, bad := range []string{"", "../x", "a/b", "a\\b", "a b"} {
		if err := st.Append(bad, model.JSONMap{"v": 1}); err == nil {
			t.Fatalf("Append(%q) succeeded, want error", bad)
		}
	}
}

// TestOpenError: a file in place of a directory fails cleanly.
func TestOpenError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(dir, []byte("file"), 0o644); err != nil {
		t.Fatalf("prep: %v", err)
	}
	if _, err := Open(filepath.Join(dir, "sub")); err == nil {
		t.Fatal("Open under a file should fail")
	}
}

// TestCloseIdempotent: Close twice is fine, and a Store is still usable after
// Close fails on nothing.
func TestCloseIdempotent(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Append("a", model.JSONMap{"v": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestConcurrentAppend: concurrent Appends to one collection all land intact.
func TestConcurrentAppend(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	const goroutines, per = 8, 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				rec := model.JSONMap{"g": g, "i": i}
				if err := st.Append("conc", rec); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	total := 0
	if err := st.Scan("conc", func(raw []byte) error {
		var m model.JSONMap
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		total++
		return nil
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if total != goroutines*per {
		t.Fatalf("scanned %d rows, want %d", total, goroutines*per)
	}
	stats := st.Stats()
	if stats["conc"] != goroutines*per {
		t.Fatalf("Stats = %d, want %d", stats["conc"], goroutines*per)
	}
}

// TestPropertyAppendScanInvariant: for any number of random appends the scan
// row count equals the append count and every row parses as JSON.
func TestPropertyAppendScanInvariant(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	var appended int64
	prop := testkit.Prop{
		Name: "append_count_matches_scan_count",
		Inv: func(r *testkit.R) string {
			n := int(r.Int63n(11)) // 0..10 new rows per iteration
			for i := 0; i < n; i++ {
				if err := st.Append("prop", r.Map(2, 4, 4)); err != nil {
					return "append: " + err.Error()
				}
			}
			appended += int64(n)
			var seen int64
			var jsonOK = true
			err := st.Scan("prop", func(raw []byte) error {
				seen++
				var v any
				if uerr := json.Unmarshal(raw, &v); uerr != nil {
					jsonOK = false
				}
				return nil
			})
			if err != nil {
				return "scan: " + err.Error()
			}
			if seen != appended {
				return fmt.Sprintf("scan saw %d rows after %d appends", seen, appended)
			}
			if !jsonOK {
				return "some row is not valid JSON"
			}
			if stats := st.Stats(); stats["prop"] != seen {
				return fmt.Sprintf("Stats[prop]=%d, scanned %d", stats["prop"], seen)
			}
			return ""
		},
	}
	testkit.Run(t, 20260825, 60, []testkit.Prop{prop})
}
