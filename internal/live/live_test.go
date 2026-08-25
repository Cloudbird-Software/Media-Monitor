package live

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/protoio"
)

// roomID is the shared fixture room id used across event assertions.
const roomID = "76600000000123"

func testUser() []byte {
	return userPB(1001, "MS4wLjABAAAA.abc123", "测试用户", 1, "https://p3.douyinpic.com/avatar.jpg", "广东")
}

// TestResolveAndEvents drives the whole pipeline: room page fetch, room_id
// extraction, signed dial, and the full event family mapping. The downward
// payloads are hand-built protobuf bytes (fixtures_test.go).
func TestResolveAndEvents(t *testing.T) {
	page := newPageServer(t)
	var gotSig atomic.Value // signature query param observed by the ws server
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sig := r.URL.Query().Get("signature"); sig != "" {
			gotSig.Store(sig)
		}
		wsUpgrade(t, w, r, func(br *bufio.Reader, bw *bufio.Writer) error {
			msgs := [][]byte{
				messagePB(methodChat, chatPB(testUser(), "你好 world")),
				messagePB(methodGift, giftPB(userPB(2002, "MS4wLjABAAAA.giver", "送礼人", 2, "", ""), 4567, 1, "小心心", 3)),
				messagePB(methodLike, likePB(testUser(), 7, 12345)),
				messagePB(methodMember, memberPB(userPB(3003, "MS4wLjABAAAA.enterer", "进场者", 2, "", ""))),
				messagePB(methodSocial, socialPB(userPB(4004, "MS4wLjABAAAA.follower", "关注者", 0, "", ""))),
				messagePB(methodFansclub, fansclubPB(testUser(), "加入粉丝团")),
				messagePB(methodRoomRank, rankPB([][]byte{
					rankItemPB(1001, "测试用户", "1", "1000"),
					rankItemPB(2002, "送礼人", "2", "500"),
				}, 888)),
				messagePB(methodRoomStats, roomStatPB(4321)),
				messagePB(methodEmojiChat, emojiPB(testUser(), "😍")),
				messagePB(methodRoomStreamAdaptation, streamPB(1)),
				messagePB(methodRoomUserSeq, seqPB(654321)),
				messagePB(methodControl, controlPB(1, "stat")),
			}
			if err := writeFrameBytes(bw, 0x2, responsePB(msgs, "cursor-1", 1756000000, "acktok-1")); err != nil {
				return err
			}
			// Wait for the client ack, then end the stream properly.
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				op, p, err := readFrameBytes(br)
				if err != nil {
					return err
				}
				if op != 0x2 {
					continue
				}
				f, ok := DecodePushFrame(p)
				if ok && f.PayloadType == "ack" {
					break
				}
			}
			return writeFrameBytes(bw, 0x2, responsePB([][]byte{
				messagePB(methodControl, controlPB(3, "bye")),
			}, "cursor-9", 1756000001, "acktok-9"))
		})
	}))
	defer ws.Close()
	envFor(t, page.URL, ws.URL)

	cfg := testConnector(t)
	ch, handler := collectEvents()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cfg.Connect(ctx, "https://live.douyin.com/roomweb1", handler); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if sig, _ := gotSig.Load().(string); sig != "test-sig" {
		t.Errorf("ws query signature = %q, want %q", sig, "test-sig")
	}

	events := drainEvents(ch)
	if len(events) != 12 {
		t.Fatalf("got %d events, want 12: %+v", len(events), events)
	}

	// chat
	ev := events[0]
	if ev.Event != "chat" {
		t.Fatalf("first event = %q", ev.Event)
	}
	if ev.Content != "你好 world" || ev.Time != 1756000000 || ev.RoomID != roomID {
		t.Errorf("chat = %+v", ev)
	}
	u := ev.User
	if u.UID != "1001" || u.SecUID != "MS4wLjABAAAA.abc123" || u.Nickname != "测试用户" ||
		u.Gender != 1 || u.AvatarURL != "https://p3.douyinpic.com/avatar.jpg" || u.IPLabel != "广东" {
		t.Errorf("chat user = %+v", u)
	}

	// gift
	ev = events[1]
	if ev.Event != "gift" || ev.Count != 3 || ev.Content != "小心心" {
		t.Errorf("gift = %+v", ev)
	}
	if ev.RawSummary["gift_id"] != uint64(4567) || ev.RawSummary["repeat_count"] != uint64(3) || ev.RawSummary["gift_count"] != uint64(1) {
		t.Errorf("gift raw = %+v", ev.RawSummary)
	}
	if ev.User.UID != "2002" {
		t.Errorf("gift giver = %+v", ev.User)
	}

	// like
	ev = events[2]
	if ev.Event != "like" || ev.Count != 7 || ev.RawSummary["total"] != uint64(12345) {
		t.Errorf("like = %+v", ev)
	}

	// enter
	ev = events[3]
	if ev.Event != "enter" || ev.User.UID != "3003" || ev.User.Gender != 2 {
		t.Errorf("enter = %+v", ev)
	}

	// follow
	ev = events[4]
	if ev.Event != "follow" || ev.User.UID != "4004" {
		t.Errorf("follow = %+v", ev)
	}

	// fansclub
	ev = events[5]
	if ev.Event != "fansclub" || ev.Content != "加入粉丝团" {
		t.Errorf("fansclub = %+v", ev)
	}

	// rank
	ev = events[6]
	if ev.Event != "rank" {
		t.Errorf("rank event = %+v", ev)
	}
	ranks, _ := ev.RawSummary["ranks"].([]any)
	if len(ranks) != 2 {
		t.Errorf("rank items = %v", ev.RawSummary)
	} else {
		r0 := ranks[0].(map[string]any)
		if r0["rank"] != "1" || r0["score"] != "1000" || r0["user_id"] != uint64(1001) || r0["user_name"] != "测试用户" {
			t.Errorf("rank[0] = %v", r0)
		}
	}
	if ev.RawSummary["total_user_count"] != uint64(888) {
		t.Errorf("rank total = %v", ev.RawSummary["total_user_count"])
	}

	// room_stat
	ev = events[7]
	if ev.Event != "room_stat" || ev.Count != 4321 {
		t.Errorf("room_stat = %+v", ev)
	}

	// emoji
	ev = events[8]
	if ev.Event != "emoji" || ev.Content != "😍" {
		t.Errorf("emoji = %+v", ev)
	}

	// stream
	ev = events[9]
	if ev.Event != "stream" || ev.RawSummary["action"] != uint64(1) {
		t.Errorf("stream = %+v", ev)
	}

	// seq
	ev = events[10]
	if ev.Event != "seq" || ev.Count != 654321 {
		t.Errorf("seq = %+v", ev)
	}

	// terminal control (status 3): delivered as the final event, ends the session
	ev = events[11]
	if ev.Event != "control" || ev.RawSummary["status"] != int64(3) || ev.Content != "bye" || ev.Time != 1756000001 {
		t.Errorf("control = %+v", ev)
	}
}

// TestGzipResponse: compressed frames are transparently decompressed.
func TestGzipResponse(t *testing.T) {
	page := newPageServer(t)
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsUpgrade(t, w, r, func(br *bufio.Reader, bw *bufio.Writer) error {
			raw := responsePB([][]byte{
				messagePB(methodChat, chatPB(testUser(), "gzipped hello")),
				messagePB(methodControl, controlPB(3, "")),
			}, "cg", 0, "")
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			if _, err := zw.Write(raw); err != nil {
				return err
			}
			if err := zw.Close(); err != nil {
				return err
			}
			return writeFrameBytes(bw, 0x2, buf.Bytes())
		})
	}))
	defer ws.Close()
	envFor(t, page.URL, ws.URL)

	ch, handler := collectEvents()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := testConnector(t).Connect(ctx, "live.douyin.com/gzroom", handler); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	events := drainEvents(ch)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Content != "gzipped hello" {
		t.Errorf("content = %q", events[0].Content)
	}
}

// TestHeartbeatAndAck: the server observes at least two "hb" PushFrames, then
// an "ack" echoing internal_ext, proving both keepalive paths.
func TestHeartbeatAndAck(t *testing.T) {
	page := newPageServer(t)
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsUpgrade(t, w, r, func(br *bufio.Reader, bw *bufio.Writer) error {
			var hb int
			var ackSeen bool
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				op, p, err := readFrameBytes(br)
				if err != nil {
					return err
				}
				if op != 0x2 {
					continue
				}
				f, ok := DecodePushFrame(p)
				if !ok {
					continue
				}
				switch f.PayloadType {
				case "hb":
					hb++
					if hb == 2 && !ackSeen {
						// Wake the client with a real response, then demand an ack.
						if err := writeFrameBytes(bw, 0x2, responsePB([][]byte{
							messagePB(methodChat, chatPB(testUser(), "hb-woken")),
						}, "c2", 0, "acktok-7")); err != nil {
							return err
						}
					}
				case "ack":
					ackSeen = true
					if string(f.Payload) != "acktok-7" {
						return fmt.Errorf("ack payload = %q, want acktok-7", f.Payload)
					}
					return writeFrameBytes(bw, 0x2, responsePB([][]byte{
						messagePB(methodControl, controlPB(3, "")),
					}, "c3", 0, ""))
				}
			}
			return errors.New("timed out waiting for hb/ack frames")
		})
	}))
	defer ws.Close()
	envFor(t, page.URL, ws.URL)

	cfg := testConnector(t)
	cfg.HeartbeatInterval = 30 * time.Millisecond
	ch, handler := collectEvents()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cfg.Connect(ctx, "https://live.douyin.com/hbroom", handler); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	events := drainEvents(ch)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (chat + control)", len(events))
	}
	if events[0].Event != "chat" || events[0].Content != "hb-woken" {
		t.Errorf("first event = %+v", events[0])
	}
	if got := cfg.Obs.Get("live.hb"); got < 2 {
		t.Errorf("obs live.hb = %d, want >= 2", got)
	}
	if got := cfg.Obs.Get("live.ack"); got != 1 {
		t.Errorf("obs live.ack = %d, want 1", got)
	}
}

// TestControlEndOnly: a control status==3 response ends Connect cleanly with
// no events delivered.
func TestControlEndOnly(t *testing.T) {
	page := newPageServer(t)
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsUpgrade(t, w, r, func(br *bufio.Reader, bw *bufio.Writer) error {
			return writeFrameBytes(bw, 0x2, responsePB([][]byte{
				messagePB(methodControl, controlPB(3, "done")),
			}, "", 0, ""))
		})
	}))
	defer ws.Close()
	envFor(t, page.URL, ws.URL)

	ch, handler := collectEvents()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := testConnector(t).Connect(ctx, "https://www.live.douyin.com/ctrlroom", handler); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if events := drainEvents(ch); len(events) != 1 || events[0].Event != "control" {
		t.Fatalf("got %d events, want exactly the terminal control event: %+v", len(events), events)
	}
}

// TestErrRoomEnd: the handler sentinel stops monitoring with a nil error and
// the server observes the close frame.
func TestErrRoomEnd(t *testing.T) {
	page := newPageServer(t)
	sawClose := make(chan struct{}, 1)
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsUpgrade(t, w, r, func(br *bufio.Reader, bw *bufio.Writer) error {
			if err := writeFrameBytes(bw, 0x2, responsePB([][]byte{
				messagePB(methodChat, chatPB(testUser(), "stop me")),
				messagePB(methodLike, likePB(testUser(), 1, 2)),
			}, "", 0, "")); err != nil {
				return err
			}
			for {
				op, _, err := readFrameBytes(br)
				if err != nil {
					return nil
				}
				if op == 0x8 {
					select {
					case sawClose <- struct{}{}:
					default:
					}
					return nil
				}
			}
		})
	}))
	defer ws.Close()
	envFor(t, page.URL, ws.URL)

	stopped := make(chan model.LiveEvent, 4)
	handler := func(ev model.LiveEvent) error {
		stopped <- ev
		return ErrRoomEnd
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := testConnector(t).Connect(ctx, "https://live.douyin.com/handlerroom", handler); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	select {
	case <-sawClose:
	case <-time.After(3 * time.Second):
		t.Error("server never observed the close frame")
	}
	if ev := <-stopped; ev.Event != "chat" {
		t.Errorf("first event = %+v", ev)
	}
}

// TestHandlerErrorFatal: a non-sentinel handler error aborts Connect without
// reconnecting (no redial storm for a broken consumer).
func TestHandlerErrorFatal(t *testing.T) {
	page := newPageServer(t)
	var accepts atomic.Int32
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepts.Add(1)
		wsUpgrade(t, w, r, func(br *bufio.Reader, bw *bufio.Writer) error {
			// Send a chat so the handler is reached, then idle.
			return writeFrameBytes(bw, 0x2, responsePB([][]byte{
				messagePB(methodChat, chatPB(testUser(), "boom")),
			}, "", 0, ""))
		})
	}))
	defer ws.Close()
	envFor(t, page.URL, ws.URL)

	handler := func(ev model.LiveEvent) error { return errors.New("consumer broke") }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := testConnector(t)
	cfg.ReconnectMax = 5
	err := cfg.Connect(ctx, "https://live.douyin.com/fatalroom", handler)
	if err == nil || !errors.Is(err, errHandlerFatal) {
		t.Fatalf("Connect = %v, want errHandlerFatal", err)
	}
	if n := accepts.Load(); n != 1 {
		t.Errorf("accepts = %d, want 1 (no reconnect after handler failure)", n)
	}
}

// TestReconnect: the first accepted socket is dropped immediately; the client
// reconnects and the second session serves events to a clean control end.
func TestReconnect(t *testing.T) {
	page := newPageServer(t)
	var accepts atomic.Int32
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := accepts.Add(1)
		wsUpgrade(t, w, r, func(br *bufio.Reader, bw *bufio.Writer) error {
			if n == 1 {
				// First session: die right after the handshake.
				return errExpectedDrop
			}
			if err := writeFrameBytes(bw, 0x2, responsePB([][]byte{
				messagePB(methodChat, chatPB(testUser(), "alive again")),
				messagePB(methodControl, controlPB(3, "")),
			}, "", 0, "")); err != nil {
				return err
			}
			for {
				if _, _, err := readFrameBytes(br); err != nil {
					return nil
				}
			}
		})
	}))
	defer ws.Close()
	envFor(t, page.URL, ws.URL)

	cfg := testConnector(t)
	ch, handler := collectEvents()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cfg.Connect(ctx, "https://live.douyin.com/reconnectroom", handler); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if n := accepts.Load(); n != 2 {
		t.Errorf("accepts = %d, want 2", n)
	}
	events := drainEvents(ch)
	if len(events) != 2 || events[0].Content != "alive again" || events[1].Event != "control" {
		t.Fatalf("events after reconnect = %+v", events)
	}
	if got := cfg.Obs.Get("live.reconnect"); got != 1 {
		t.Errorf("obs live.reconnect = %d, want 1", got)
	}
}

// TestReconnectExhausted: persistent drops burn through ReconnectMax and
// Connect returns the last error.
func TestReconnectExhausted(t *testing.T) {
	page := newPageServer(t)
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsUpgrade(t, w, r, func(br *bufio.Reader, bw *bufio.Writer) error {
			return errExpectedDrop
		})
	}))
	defer ws.Close()
	envFor(t, page.URL, ws.URL)

	cfg := testConnector(t)
	cfg.ReconnectMax = 1
	ch, handler := collectEvents()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := cfg.Connect(ctx, "https://live.douyin.com/droproom", handler)
	if err == nil {
		t.Fatal("Connect returned nil, want reconnect-limit error")
	}
	if !strings.Contains(err.Error(), "reconnect") {
		t.Errorf("error = %v, want reconnect-limit message", err)
	}
	if events := drainEvents(ch); len(events) != 0 {
		t.Errorf("unexpected events: %+v", events)
	}
}

// TestFailClosedSigner: Connect refuses to run without a signature signer.
func TestFailClosedSigner(t *testing.T) {
	cfg := testConnector(t)
	cfg.Signer = nil
	_, handler := collectEvents()
	err := cfg.Connect(context.Background(), "https://live.douyin.com/x", handler)
	if err == nil || !strings.Contains(err.Error(), "refusing to connect unsigned") {
		t.Fatalf("Connect = %v, want unsigned-refusal", err)
	}
}

// TestConnectValidation: bad inputs fail closed before any network I/O.
func TestConnectValidation(t *testing.T) {
	_, handler := collectEvents()
	badHost := testConnector(t)
	if err := badHost.Connect(context.Background(), "https://evil.example/x", handler); err == nil ||
		!strings.Contains(err.Error(), "unsupported room url host") {
		t.Errorf("foreign host = %v", err)
	}
	if err := badHost.Connect(context.Background(), "https://live.douyin.com/", handler); err == nil {
		t.Errorf("empty room_web accepted")
	}
	empty := testConnector(t)
	if err := empty.Connect(context.Background(), "  ", handler); err == nil {
		t.Errorf("blank room url accepted")
	}
	noMeta := testConnector(t)
	noMeta.Registry = contracts.NewRegistry() // no douyin-meta loaded
	if err := noMeta.Connect(context.Background(), "https://live.douyin.com/x", handler); err == nil ||
		!strings.Contains(err.Error(), metaContractName) {
		t.Errorf("missing contract = %v", err)
	}
	if err := badHost.Connect(context.Background(), "https://live.douyin.com/a/b/c", nil); err == nil {
		t.Errorf("nil handler accepted")
	}
}

// TestRoomIDExtraction covers the runtime locator semantics on embedded HTML
// fragments (camelCase key derived from the contract binding field name).
func TestRoomIDExtraction(t *testing.T) {
	cases := []struct {
		name      string
		page      string
		fieldName string
		want      string
		wantErr   bool
	}{
		{"plain json string", `{"roomId":"76600000000123"}`, "room_id", "76600000000123", false},
		{"whitespace tolerant", `<p>"roomId"   :   "766111"</p>`, "room_id", "766111", false},
		{"inside larger html", `<script>var s={"roomId":"766222","x":1};</script>`, "room_id", "766222", false},
		{"first match wins", `{"roomId":"766333","roomId":"766444"}`, "room_id", "766333", false},
		{"missing key", `<div>no room here</div>`, "room_id", "", true},
		{"non-digit value", `{"roomId":"12ab34"}`, "room_id", "", true},
		{"empty value", `{"roomId":""}`, "room_id", "", true},
		{"too many digits", `{"roomId":"123456789012345678901"}`, "room_id", "", true},
		{"camel field other", `{"userCount":"42"}`, "user_count", "42", false},
		{"single word field", `{"roomid":"7"}`, "roomid", "7", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extractRoomID([]byte(c.page), c.fieldName)
			if c.wantErr {
				if err == nil {
					t.Fatalf("extractRoomID = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractRoomID: %v", err)
			}
			if got != c.want {
				t.Errorf("extractRoomID = %q, want %q", got, c.want)
			}
		})
	}
}

// TestNormalizeRoomURL: validation and canonicalization of the room URL
// against the douyin-meta contract path template.
func TestNormalizeRoomURL(t *testing.T) {
	reg := testRegistry(t)
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"canonical", "https://live.douyin.com/123abc", "https://live.douyin.com/123abc/", false},
		{"no scheme", "live.douyin.com/7654321", "https://live.douyin.com/7654321/", false},
		{"www alias", "https://www.live.douyin.com/ab", "https://live.douyin.com/ab/", false},
		{"first segment", "https://live.douyin.com/a/b", "https://live.douyin.com/a/", false},
		{"query ignored", "https://live.douyin.com/x?modal_id=1", "https://live.douyin.com/x/", false},
		{"digits room", "https://live.douyin.com/76600000000123", "https://live.douyin.com/76600000000123/", false},
		{"foreign host", "https://m.douyin.com/x", "", true},
		{"empty host path", "https://live.douyin.com/", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeRoomURL(reg, c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("normalizeRoomURL = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeRoomURL: %v", err)
			}
			if got != c.want {
				t.Errorf("normalizeRoomURL = %q, want %q", got, c.want)
			}
		})
	}
}

// TestBuildWSSURL verifies the wss URL shape: static template, random
// user_unique_id, cursor, and the appended signature.
func TestBuildWSSURL(t *testing.T) {
	var gotParams map[string]string
	var gotQuery string
	sign := func(urlQuery string, params map[string]string) (string, error) {
		gotQuery = urlQuery
		gotParams = params
		return "SIG-1", nil
	}
	u, err := buildWSSURL(roomID, "cur-42", sign)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "wss://") || !strings.Contains(u, "/webcast/im/push/v2/?") {
		t.Errorf("scheme/path = %s", u)
	}
	vals, err := url.ParseQuery(u[strings.IndexByte(u, '?')+1:])
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]bool{
		"app_name":            vals.Get("app_name") == "douyin_web",
		"version_code":        vals.Get("version_code") == "180800",
		"compress":            vals.Get("compress") == "gzip",
		"room_id":             vals.Get("room_id") == roomID,
		"cursor param":        vals.Has("cursor") && vals.Get("cursor") == "cur-42",
		"unique id 19 digits": len(vals.Get("user_unique_id")) == 19 && allDigits(vals.Get("user_unique_id")),
		"signature":           vals.Get("signature") == "SIG-1",
	}
	for k, ok := range checks {
		if !ok {
			t.Errorf("%s check failed: %s", k, u)
		}
	}
	if gotParams["room_id"] != roomID || gotParams["cursor"] != "cur-42" {
		t.Errorf("signer params = %v", gotParams)
	}
	if !strings.HasPrefix(gotQuery, "app_name=douyin_web&") || strings.Contains(gotQuery, "signature=") {
		t.Errorf("signer query = %q", gotQuery)
	}

	// The last token of the query must be the signature (paranoia: order).
	if !strings.HasSuffix(u, "&signature=SIG-1") {
		t.Errorf("signature not last: %s", u)
	}
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// TestMaybeGunzip covers the magic-byte detection and passthrough.
func TestMaybeGunzip(t *testing.T) {
	raw := []byte("plain bytes, no gzip magic at the start")
	got, err := maybeGunzip(raw)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("passthrough = %q, %v", got, err)
	}
	payload := []byte("compressed payload")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	got, err = maybeGunzip(buf.Bytes())
	if err != nil || string(got) != string(payload) {
		t.Fatalf("gunzip = %q, %v", got, err)
	}
	if _, err := maybeGunzip([]byte{0x1f, 0x8b, 0x00, 0x01, 0x02}); err == nil {
		t.Fatal("truncated gzip accepted")
	}
}

// TestDecodeResponseFields round-trips the response envelope fields.
func TestDecodeResponseFields(t *testing.T) {
	raw := responsePB([][]byte{
		messagePB(methodChat, chatPB(testUser(), "fields")),
		messagePB(methodLike, likePB(testUser(), 1, 2)),
	}, "cur-9", 1756000022, "ie-5")
	// params entry (field 7 map)
	raw = append(raw, pb(func(w *protoio.Writer) {
		w.Bytes(7, pb(func(w2 *protoio.Writer) {
			w2.Bytes(1, []byte("k"))
			w2.Bytes(2, []byte("v"))
		}))
	})...)
	resp, err := DecodeResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Cursor != "cur-9" || resp.Now != 1756000022 || resp.InternalExt != "ie-5" {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Params["k"] != "v" {
		t.Errorf("params = %v", resp.Params)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("messages = %d", len(resp.Messages))
	}
	method, payload, ok := DecodeMessage(resp.Messages[0])
	if !ok || method != methodChat {
		t.Fatalf("message = %q,%v", method, ok)
	}
	ev, status, recognized := EventFromMessage(roomID, method, payload, 0)
	if !recognized || ev.Event != "chat" || ev.Content != "fields" || status != 0 {
		t.Errorf("event = %+v status=%d", ev, status)
	}
	// Unknown method is skipped silently.
	if _, _, recognized := EventFromMessage(roomID, "WebcastSomethingNew", nil, 0); recognized {
		t.Errorf("unknown method recognized")
	}
	// Corrupt trailing bytes fail closed.
	if _, err := DecodeResponse([]byte{0x00, 0x00, 0x00}); err == nil {
		t.Error("garbage response accepted")
	}
}

// TestPushFrameWireLayout pins the canonical hb/ack encodings.
func TestPushFrameWireLayout(t *testing.T) {
	hb := EncodePushFrame(PushFrame{PayloadType: "hb"})
	if len(hb) != 4+4 { // length prefix + {tag(1,w2), len, "hb"}
		t.Errorf("hb size = %d: %x", len(hb), hb)
	}
	f, ok := DecodePushFrame(hb)
	if !ok || f.PayloadType != "hb" || f.LogID != 0 || len(f.Payload) != 0 {
		t.Fatalf("hb decode = %+v, %v", f, ok)
	}
	ack := EncodePushFrame(PushFrame{PayloadType: "ack", LogID: 0, Payload: []byte("token")})
	f, ok = DecodePushFrame(ack)
	if !ok || f.PayloadType != "ack" || string(f.Payload) != "token" {
		t.Fatalf("ack decode = %+v, %v", f, ok)
	}
	if _, ok := DecodePushFrame([]byte{0, 0, 0, 5, 0x0a}); ok {
		t.Error("length mismatch accepted as ok")
	}
}
