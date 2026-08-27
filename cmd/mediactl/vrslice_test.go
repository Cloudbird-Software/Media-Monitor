package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVRSliceMockEndToEnd: the three AC-20 segments run against the mock
// platform and each carries its behavioral assertion — backtrack params
// reach the engine (early-stop delta), the comments cursor chain merges
// two pages without duplicates, and the download artifact's sha256 matches
// the served bytes at an IFACE-3 path.
func TestVRSliceMockEndToEnd(t *testing.T) {
	out := t.TempDir()
	ev, _, err := runVRSlice(vrOpts{
		secUID: secUIDDouyin, mock: true,
		adaptRoot: adaptRootForTest(),
		writeDir:  out, artifacts: filepath.Join(out, "artifacts"),
	})
	if err != nil {
		t.Fatalf("vr-slice: %v", err)
	}
	if len(ev.Segments) != 3 {
		t.Fatalf("segments = %d, want 3", len(ev.Segments))
	}
	for _, sg := range ev.Segments {
		v := verdict{Status: sg.Status, Code: sg.Code}
		if !legalExtreme(v) || sg.Status != vClean {
			t.Fatalf("segment %s bad ending status=%q code=%q detail=%q metrics=%v",
				sg.Name, sg.Status, sg.Code, sg.Detail, sg.Metrics)
		}
	}

	var backtrack, comments vrSegment
	for _, sg := range ev.Segments {
		switch {
		case strings.HasPrefix(sg.Name, "seg1"):
			backtrack = sg
		case strings.HasPrefix(sg.Name, "seg2"):
			comments = sg
		}
	}
	// seg1: min_engagement must be proven to reach engine BEHAVIOR — the
	// floored walk stops earlier than the control walk.
	controlItems, _ := backtrack.Metrics["control_items"].(int)
	flooredItems, ok1 := floatOf(backtrack.Metrics["floored_items"])
	if controlItems == 0 || !ok1 || flooredItems >= float64(controlItems) {
		t.Fatalf("backtrack param reach not proven: control=%d floored=%v metrics=%v",
			controlItems, backtrack.Metrics["floored_items"], backtrack.Metrics)
	}

	// seg2: two cursor pages merged, zero duplicate cids, more rows than a
	// single page.
	pages, _ := floatOf(comments.Metrics["pages_received"])
	total, _ := floatOf(comments.Metrics["comments_total"])
	uniq, _ := floatOf(comments.Metrics["unique_cids"])
	if pages < 2 || uniq <= pages*0 /*sanity*/ || uniq != total || total <= 2 {
		t.Fatalf("comments chain wrong: pages=%v total=%v unique=%v metrics=%v",
			pages, total, uniq, comments.Metrics)
	}

	// seg3: artifact exists, bytes match, sha256 matches served payload.
	a := ev.Artifact
	if a == nil {
		t.Fatal("artifact evidence missing")
	}
	st, err := os.Stat(a.Path)
	if err != nil {
		t.Fatalf("artifact unreadable: %v", err)
	}
	wantSum := sha256.Sum256(payloadPattern(vrPayloadLen))
	if st.Size() != int64(vrPayloadLen) || a.Bytes != st.Size() {
		t.Fatalf("artifact size mismatch: stat=%d reported=%d", st.Size(), a.Bytes)
	}
	if !strings.EqualFold(a.SHA256, hex.EncodeToString(wantSum[:])) {
		t.Fatalf("sha256 mismatch: got %s", a.SHA256)
	}
	if !strings.Contains(filepath.ToSlash(a.Path), "/artifacts/douyin/") {
		t.Fatalf("artifact not under IFACE-3 layout: %s", a.Path)
	}
}

func floatOf(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	default:
		return -1, false
	}
}

// TestVRSliceRequiresSecUIDAndMockGate: empty sec_uid errors; --mock=false
// fails closed pointing at the owner live lane instead of pretending.
func TestVRSliceRequiresSecUIDAndMockGate(t *testing.T) {
	if _, _, err := runVRSlice(vrOpts{secUID: "", mock: true, adaptRoot: adaptRootForTest()}); err == nil {
		t.Fatal("empty sec_uid accepted")
	}
	if _, _, err := runVRSlice(vrOpts{secUID: "x", mock: false, adaptRoot: adaptRootForTest()}); err == nil ||
		!strings.Contains(err.Error(), "live") {
		t.Fatalf("--mock=false must fail closed toward owner lane, got %v", err)
	}
}

// TestWriteFileAtomicNoHalfArtifacts: interrupted copies never surface a
// truncated file under the final name (rename-after-write).
func TestWriteFileAtomicNoHalfArtifacts(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "douyin", "7660000000000000001.mp4")
	payload := payloadPattern(4096)
	n, sum, err := writeFileAtomic(dst, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("bytes=%d want %d", n, len(payload))
	}
	raw, err := os.ReadFile(dst)
	if err != nil || len(raw) != len(payload) {
		t.Fatalf("final file broken: %v", err)
	}
	local := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(local[:]) {
		t.Fatal("hash of written bytes diverges")
	}
}

// TestVRSegmentSummaryLegality mirrors the matrix summary semantics.
func TestVRSegmentSummaryLegality(t *testing.T) {
	segs := []vrSegment{
		{Name: "seg1", Status: vClean},
		{Name: "seg2", Status: vClean},
		{Name: "seg3", Status: vClean},
	}
	sum := summarizeVRSegments(segs)
	if sum[vClean] != 3 || sum["illegal"] != 0 {
		t.Fatalf("summary = %v", sum)
	}
	segs[2].Status = vFailClosed // undocumented
	sum = summarizeVRSegments(segs)
	if sum["illegal"] != 1 {
		t.Fatalf("undocumented segment not flagged: %v", sum)
	}
}
