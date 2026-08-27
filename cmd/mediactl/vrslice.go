// vrslice.go — the VR consumption vertical-slice verifier
// (`mediactl lab vr-slice --sec-uid <id>`): executes the three-segment
// slice end to end against a fixture-driven mock platform and asserts
// exactly what AC-20 needs of MM's side:
//
//	seg1 user_posts backtrack: window/min_engagement reach the ENGINE's
//	     behavior (early-stop delta vs an unconditioned control walk)
//	seg2 comments cursor chain >=2 pages merged without duplicates
//	seg3 download: resolve_video -> streamed bytes land at an IFACE-3
//	     artifact path with sha256 equal to the served payload
//
// Evidence JSON lands at adapt/reports/vr-slice-evidence-<ts>.json next to
// the run's artifacts/ tree. --mock defaults to true; live joint-run
// evidence belongs to the owner (this tool refuses --mock=false until that
// lane exists — fail-closed, never pretending).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

const (
	vrCard       = "W8-C2"
	vrIR         = "IR-MM-0001/AC-20"
	vrPlatform   = douyinP // the mock slice walks the platform declaring all three atoms
	vrPayloadLen = 64 * 1024
)

// vrSegment is one slice leg's judgment (three-valued like the matrix).
type vrSegment struct {
	Name    string         `json:"name"`
	Status  string         `json:"status"`
	Code    string         `json:"code,omitempty"`
	Detail  string         `json:"detail"`
	Metrics map[string]any `json:"metrics,omitempty"`
}

// artifactInfo is the IFACE-3 result shape VR consumes cross-process.
type artifactInfo struct {
	Platform string `json:"platform"`
	ItemID   string `json:"item_id"`
	Path     string `json:"path"` // absolute; same-machine VR-readable convention
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
}

// vrEvidence is the archived integration record.
type vrEvidence struct {
	Card       string         `json:"card"`
	IR         string         `json:"ir"`
	Tool       string         `json:"tool"`
	Mode       string         `json:"mode"`
	SecUID     string         `json:"sec_uid"`
	StartedAt  string         `json:"started_at"`
	FinishedAt string         `json:"finished_at"`
	EnvNote    string         `json:"env_note"`
	Segments   []vrSegment    `json:"segments"`
	Artifact   *artifactInfo  `json:"artifact,omitempty"`
	Summary    map[string]int `json:"summary"`
}

type vrOpts struct {
	secUID    string
	mock      bool
	adaptRoot string
	writeDir  string // "" disables the evidence file
	artifacts string // artifact root for the run ("<run>/artifacts")
}

// runVRSlice executes the three segments on a fresh mock session.
func runVRSlice(o vrOpts) (vrEvidence, string, error) {
	if o.secUID == "" {
		return vrEvidence{}, "", errors.New("vr-slice: --sec-uid is required")
	}
	if !o.mock {
		return vrEvidence{}, "", errors.New(
			"vr-slice: live lane (--mock=false) requires the owner joint-run environment " +
				"(VR transport adapter + real accounts); refusing to pretend (INV-4)")
	}
	ev := vrEvidence{
		Card: vrCard, IR: vrIR, Tool: "mediactl lab vr-slice",
		Mode: "offline_mock", SecUID: o.secUID,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		EnvNote: "mock-driven end-to-end slice: VR-side transport/INV-3 assertions belong to the VR repo card; " +
			"live joint-run evidence belongs to the owner. This record proves MM's three atoms behave as contracted.",
	}
	mp := newMockPlatform()
	defer mp.Close()
	mp.Start()
	rc := newRowCtx(mp, o.adaptRoot)
	rc.artRoot = o.artifacts

	for _, seg := range []func(*rowCtx) (vrSegment, *artifactInfo){
		vrSeg1Backtrack, vrSeg2CommentsChain, vrSeg3Download,
	} {
		sg, art := seg(rc)
		ev.Segments = append(ev.Segments, sg)
		if art != nil {
			ev.Artifact = art
		}
	}
	ev.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	ev.Summary = summarizeVRSegments(ev.Segments)

	path := ""
	if o.writeDir != "" {
		p, werr := writeReportTo(o.writeDir, "vr-slice", "evidence", ev)
		if werr != nil {
			return ev, p, werr
		}
		path = p
	}
	return ev, path, nil
}

func cmdLabVRSlice(args []string) error {
	fs := flag.NewFlagSet("lab vr-slice", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "use: lab vr-slice --sec-uid <id> [--mock=true] [--adapt-dir DIR] [--write-dir DIR]\n")
	}
	secUID := fs.String("sec-uid", "", "creator id to walk (douyin sec_user_id)")
	mock := fs.Bool("mock", true, "fixture-driven mock platform (default true; false = owner live lane, refused here)")
	adaptOverride := fs.String("adapt-dir", "", "adapt root override")
	writeDir := fs.String("write-dir", "", "evidence output dir (default <adapt-dir>/reports)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *secUID == "" {
		fs.Usage()
		return errors.New("--sec-uid is required")
	}
	root := *adaptOverride
	if root == "" {
		root = adaptDir()
	}
	dir := *writeDir
	if dir == "" {
		dir = filepath.Join(root, "reports")
	}
	ev, _, err := runVRSlice(vrOpts{secUID: *secUID, mock: *mock,
		adaptRoot: root, writeDir: dir,
		artifacts: filepath.Join(dir, "artifacts")})
	if err != nil {
		return err
	}
	for _, seg := range ev.Segments {
		fmt.Printf("  %-40s %-16s %s\n", seg.Name, seg.Status, firstLine(seg.Detail))
	}
	fmt.Printf("summary: total=%d clean_success=%d illegal=%d\n",
		ev.Summary["total"], ev.Summary[vClean], ev.Summary["illegal"])
	if ev.Artifact != nil {
		fmt.Println("artifact:", ev.Artifact.Path)
		fmt.Println("sha256: ", ev.Artifact.SHA256)
	}
	if ev.Summary["illegal"] > 0 {
		return fmt.Errorf("vr-slice: %d segment(s) ended outside the legal three-valued set", ev.Summary["illegal"])
	}
	return nil
}

// payloadPattern builds deterministic download source bytes so the sha256
// assertion compares against known-good data rather than an echo.
func payloadPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// writeFileAtomic streams r through tmp then renames onto dst — no half
// artifacts visible even if the process dies mid-write.
func writeFileAtomic(dst string, r io.Reader) (int64, [sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, sum, err
	}
	tmp, err := os.CreateTemp(dir, ".vr-dl-*")
	if err != nil {
		return 0, sum, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		tmp.Close()
		return 0, sum, err
	}
	if err := tmp.Close(); err != nil {
		return n, sum, err
	}
	copy(sum[:], h.Sum(nil))
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return n, sum, err
	}
	return n, sum, nil
}

// summarizeVRSegments folds segment verdicts into the evidence summary;
// anything outside the legal triple counts illegal and blocks exit 0.
func summarizeVRSegments(segs []vrSegment) map[string]int {
	sum := map[string]int{"total": len(segs), vClean: 0, vSkip: 0, vFailClosed: 0, "illegal": 0}
	for _, sg := range segs {
		v := verdict{Status: sg.Status, Code: sg.Code}
		switch {
		case !legalExtreme(v):
			sum["illegal"]++
		case sg.Status == vClean:
			sum[vClean]++
		case sg.Status == vSkip:
			sum[vSkip]++
		default:
			sum[vFailClosed]++
		}
	}
	return sum
}

// failSeg normalizes a segment failure into its verdict shape.
func failSeg(name string, err error) vrSegment {
	return vrSegment{Name: name, Status: vFailClosed,
		Code: "", Detail: err.Error()}
}

// --- segments ---------------------------------------------------------------

// vrSeg1Backtrack proves window/min_engagement reach engine BEHAVIOR: the
// unconditioned control walk traverses every fixture page while the floored
// walk early-stops — the delta is the reachability evidence AC-1 asks for.
func vrSeg1Backtrack(rc *rowCtx) (vrSegment, *artifactInfo) {
	const name = "seg1-user-posts-backtrack-params-reach-engine"
	if _, err := addUserPostsChain(rc); err != nil {
		return failSeg(name, err), nil
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return failSeg(name, err), nil
	}
	controlItems, _, err := eng.UserPosts(rc.ctx, douyinP, secUIDDouyin, model.Cursor{}, 0, collect.BacktrackOptions{})
	if err != nil {
		return failSeg(name, err), nil
	}
	flooredItems, flooredCur, err := eng.UserPosts(rc.ctx, douyinP, secUIDDouyin, model.Cursor{}, 0, collect.BacktrackOptions{
		MinEngagement:        &collect.EngagementFloor{Metric: "digg", Threshold: 170000},
		StopAfterConsecutive: 2,
	})
	if err != nil {
		return failSeg(name, err), nil
	}
	flooredN, controlN := len(flooredItems), len(controlItems)
	if flooredN == 0 || flooredN >= controlN {
		return failSeg(name, fmt.Errorf("floored walk (%d items) failed to stop earlier than control (%d items)", flooredN, controlN)), nil
	}
	for _, it := range flooredItems {
		if it.Stats.Digg >= 170000 {
			return failSeg(name, fmt.Errorf("item above floor surfaced before early stop (digg=%d)", it.Stats.Digg)), nil
		}
	}
	if !flooredCur.HasMore {
		return failSeg(name, errors.New("early-stop cursor must stay resumable")), nil
	}
	return vrSegment{
		Name: name, Status: vClean,
		Detail: fmt.Sprintf("min_engagement reached the engine: control %d items vs floored early-stop at %d items (consecutive-low rule fired inside collect.UserPosts; cursor resumable)",
			controlN, flooredN),
		Metrics: map[string]any{
			"control_items": controlN,
			"floored_items": flooredN,
			"min_engagement": map[string]any{
				"metric": "digg", "threshold": 170000,
			},
			"stop_after_consecutive": 2,
			"param_reach_evidence":   "early-stop-delta",
			"resumable_after_stop":   flooredCur.HasMore,
		},
	}, nil
}

// vrSeg2CommentsChain drives get_comments through explicit cursor
// pass-back continuations (the exact shape VR's transport adapter repeats):
// each leg asks for one page worth of rows and hands the returned cursor
// back until has_more ends the chain; the merged result must be duplicate-
// free and strictly larger than one page.
func vrSeg2CommentsChain(rc *rowCtx) (vrSegment, *artifactInfo) {
	const name = "seg2-comments-cursor-chain-dedupe"
	path, err := commentsChain(rc, douyinP, 2)
	if err != nil {
		return failSeg(name, err), nil
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return failSeg(name, err), nil
	}
	pageSize := 2 // exactly the fixture's page size: one page per leg
	seen := make(map[string]bool, 8)
	var order []string
	cur := model.Cursor{}
	legs := 0
	for legs < 5 {
		cs, next, err := eng.ItemComments(rc.ctx, douyinP, itemIDDouyinUP, cur, pageSize)
		if err != nil {
			return failSeg(name, err), nil
		}
		legs++
		for _, c := range cs {
			if seen[c.CID] {
				return failSeg(name, fmt.Errorf("duplicate cid %s across cursor continuations", c.CID)), nil
			}
			seen[c.CID] = true
			order = append(order, c.CID)
		}
		cur = next
		if !next.HasMore {
			break
		}
	}
	keys := rc.mp.ReceivedKeys(path)
	if legs < 2 || len(keys) < 2 || len(seen) <= pageSize {
		return failSeg(name, fmt.Errorf(
			"cursor chain thin: legs=%d requests=%v unique=%d — need >=2 continuations merging more than one page",
			legs, keys, len(seen))), nil
	}
	return vrSegment{
		Name: name, Status: vClean,
		Detail: fmt.Sprintf("cursor pass-back chain: %d legs over %d page requests, %d comments merged (%d unique cids), zero duplicates — exceeds any single-page limit",
			legs, len(keys), len(order), len(seen)),
		Metrics: map[string]any{
			"legs": legs, "pages_received": len(keys),
			"comments_total": len(order), "unique_cids": len(seen),
			"received_keys": keys,
		},
	}, nil
}

// vrSeg3Download resolves a play address through the video-download
// contract, streams bytes locally (never over MCP), and lands them at the
// IFACE-3 artifact path with sha256 equality against the served payload.
func vrSeg3Download(rc *rowCtx) (vrSegment, *artifactInfo) {
	const name = "seg3-download-artifact-sha256"
	payload := payloadPattern(vrPayloadLen)
	wantSum := sha256.Sum256(payload)
	rc.mp.SetMedia("/video/play/", payload)
	// Serve the video-download contract itself so resolve_video has a mock
	// answer; AddFixtureChain rebases its example.invalid play_addr onto
	// this session's listener.
	vdc, err := rc.contract(douyinP, "video_download")
	if err != nil {
		return failSeg(name, err), nil
	}
	if err := rc.mp.AddFixtureChain(vdc.Transport.Path,
		filepath.Join(rc.adaptRoot, "fixtures", "douyin-video-download.1.json")); err != nil {
		return failSeg(name, err), nil
	}
	eng, err := rc.engineFor(douyinP)
	if err != nil {
		return failSeg(name, err), nil
	}
	meta, err := eng.ResolveVideo(rc.ctx, douyinP, itemIDDouyinUP)
	if err != nil {
		return failSeg(name, err), nil
	}
	if meta.URL == "" {
		return failSeg(name, errors.New("resolve returned empty play url")), nil
	}
	if mpURL := rc.mp.URL(); strings.HasPrefix(meta.URL, fixtureHostPrefix+"/") || !strings.HasPrefix(meta.URL, mpURL) {
		return failSeg(name, fmt.Errorf("play url escaped the mock session: %s", meta.URL)), nil
	}
	if rc.artRoot == "" {
		return failSeg(name, errors.New("artifact root unset")), nil
	}
	dst := filepath.Join(rc.artRoot, vrPlatform, itemIDDouyinUP+".mp4")
	n, gotSum, err := streamDownloadToFile(rc.ctx, eng, meta.URL, dst)
	if err != nil {
		return failSeg(name, err), nil
	}
	gotHex := hex.EncodeToString(gotSum[:])
	wantHex := hex.EncodeToString(wantSum[:])
	if n != int64(len(payload)) || gotHex != wantHex {
		return failSeg(name, fmt.Errorf("artifact mismatch: %d bytes sha %s, want %d bytes sha %s",
			n, gotHex, len(payload), wantHex)), nil
	}
	info := &artifactInfo{Platform: vrPlatform, ItemID: itemIDDouyinUP,
		Path: dst, Bytes: n, SHA256: gotHex}
	return vrSegment{
		Name: name, Status: vClean,
		Detail: fmt.Sprintf("resolve_video -> %dB streamed to %s (sha256 matches served payload); bytes ride local disk, not the MCP channel (IFACE-3)", n, dst),
		Metrics: map[string]any{
			"bytes": n, "sha256": gotHex,
			"iface3_layout": "artifacts/<platform>/<item_id>.mp4",
			"url_host":      "mock-session-local",
		},
	}, info
}

// streamDownloadToFile fetches url through the engine's download path into
// a temp file (hashing along the way), then renames onto dst — atomic
// publish per IFACE-3.
func streamDownloadToFile(ctx context.Context, eng *collect.Engine, url, dst string) (int64, [sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, sum, err
	}
	tmp, err := os.CreateTemp(dir, ".vr-dl-*")
	if err != nil {
		return 0, sum, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, derr := eng.Download(ctx, url, io.MultiWriter(tmp, h))
	if cerr := tmp.Close(); derr == nil {
		derr = cerr
	}
	if derr != nil {
		return 0, sum, derr
	}
	copy(sum[:], h.Sum(nil))
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return n, sum, err
	}
	return n, sum, nil
}
