package live

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/protoio"
)

const wsAcceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsAcceptGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// wsUpgrade performs the server side of the RFC 6455 handshake on a hijacked
// connection (same pattern as internal/wsutil/ws_test.go) and hands the byte
// streams to serve.
func wsUpgrade(t *testing.T, w http.ResponseWriter, r *http.Request, serve func(br *bufio.Reader, bw *bufio.Writer) error) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("response writer does not support hijacking")
		return
	}
	raw, brw, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	defer raw.Close()
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		t.Error("missing Sec-WebSocket-Key")
		return
	}
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", acceptKey(key))
	if err := brw.Flush(); err != nil {
		t.Errorf("handshake flush: %v", err)
		return
	}
	if serve != nil {
		if err := serve(brw.Reader, brw.Writer); err != nil {
			if errors.Is(err, errExpectedDrop) {
				t.Logf("serve: expected test drop (%v)", err)
			} else {
				t.Errorf("serve: %v", err)
			}
		}
	}
}

// readFrameBytes parses one (possibly masked) client frame.
func readFrameBytes(br *bufio.Reader) (op byte, payload []byte, err error) {
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return 0, nil, err
	}
	b0, b1 := h[0], h[1]
	op = b0 & 0x0f
	masked := b1&0x80 != 0
	var length uint64
	switch ln := b1 & 0x7f; {
	case ln < 126:
		length = uint64(ln)
	case ln == 126:
		var x [2]byte
		if _, err = io.ReadFull(br, x[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(x[:]))
	default:
		var x [8]byte
		if _, err = io.ReadFull(br, x[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(x[:])
	}
	var mk [4]byte
	if masked {
		if _, err = io.ReadFull(br, mk[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mk[i&3]
		}
	}
	return op, payload, nil
}

// writeFrameBytes writes one unmasked (server-side) frame.
func writeFrameBytes(bw *bufio.Writer, op byte, payload []byte) error {
	frame := make([]byte, 0, 10+len(payload))
	frame = append(frame, 0x80|op)
	switch {
	case len(payload) < 126:
		frame = append(frame, byte(len(payload)))
	case len(payload) <= 0xFFFF:
		frame = append(frame, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		var ln [8]byte
		binary.BigEndian.PutUint64(ln[:], uint64(len(payload)))
		frame = append(frame, 127)
		frame = append(frame, ln[:]...)
	}
	frame = append(frame, payload...)
	if _, err := bw.Write(frame); err != nil {
		return err
	}
	return bw.Flush()
}

// pb builds one protobuf message with the wire writer.
func pb(fn func(w *protoio.Writer)) []byte {
	var w protoio.Writer
	fn(&w)
	return w.BytesOut()
}

// ---- fixture builders (hand-written protobuf via protoio.Writer) ----

func commonPB(createTime uint64) []byte {
	return pb(func(w *protoio.Writer) { w.UInt64(commonCreateTime, createTime) })
}

func userPB(uid uint64, secUID, nickname string, gender uint64, avatarURL, ipLabel string) []byte {
	return pb(func(w *protoio.Writer) {
		if uid != 0 {
			w.UInt64(userID, uid)
		}
		if secUID != "" {
			w.Bytes(userSecUID, []byte(secUID))
		}
		if nickname != "" {
			w.Bytes(userNickname, []byte(nickname))
		}
		if gender != 0 {
			w.UInt64(userGender, gender)
		}
		if avatarURL != "" {
			w.Bytes(userAvatar, pb(func(w2 *protoio.Writer) { w2.Bytes(imgURLList, []byte(avatarURL)) }))
		}
		if ipLabel != "" {
			w.Bytes(userIPLabel, []byte(ipLabel))
		}
	})
}

func chatPB(u []byte, content string) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(chatCommon, commonPB(1700000001))
		w.Bytes(chatUser, u)
		w.Bytes(chatContent, []byte(content))
	})
}

func giftPB(u []byte, gid, count uint64, name string, repeat uint64) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(giftCommon, commonPB(1700000002))
		w.UInt64(giftID, gid)
		w.UInt64(giftCount, count)
		w.Bytes(giftName, []byte(name))
		w.Bytes(giftUser, u)
		if repeat != 0 {
			w.UInt64(giftRepeatCount, repeat)
		}
	})
}

func likePB(u []byte, count, total uint64) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(likeCommon, commonPB(1700000003))
		w.UInt64(likeCount, count)
		w.UInt64(likeTotal, total)
		w.Bytes(likeUser, u)
	})
}

func memberPB(u []byte) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(memberCommon, commonPB(1700000004))
		w.Bytes(memberUser, u)
		w.UInt64(memberCount, 42)
		w.Bytes(memberAction, []byte("enter"))
	})
}

func socialPB(u []byte) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(socialCommon, commonPB(1700000005))
		w.Bytes(socialUser, u)
	})
}

func seqPB(total uint64) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(1, commonPB(1700000006))
		w.UInt64(seqTotal, total)
	})
}

func fansclubPB(u []byte, content string) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(fansclubCommon, commonPB(1700000007))
		w.Bytes(fansclubContent, []byte(content))
		if u != nil {
			w.Bytes(fansclubUser, u)
		}
	})
}

func rankItemPB(userID uint64, name, rank, score string) []byte {
	return pb(func(w *protoio.Writer) {
		if userID != 0 {
			w.UInt64(rankItemUserID, userID)
		}
		if name != "" {
			w.Bytes(rankItemUser, []byte(name))
		}
		if rank != "" {
			w.Bytes(rankItemRank, []byte(rank))
		}
		if score != "" {
			w.Bytes(rankItemScore, []byte(score))
		}
	})
}

func rankPB(items [][]byte, totalUserCount uint64) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(1, commonPB(1700000008))
		for _, it := range items {
			w.Bytes(rankItems, it)
		}
		if totalUserCount != 0 {
			w.UInt64(rankTotalUsers, totalUserCount)
		}
	})
}

func roomStatPB(viewers uint64) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(roomStatCommon, commonPB(1700000009))
		w.UInt64(roomStatViewer, viewers)
	})
}

func controlPB(status uint64, ext string) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(controlCommon, commonPB(1700000010))
		w.UInt64(controlStatus, status)
		if ext != "" {
			w.Bytes(controlExt, []byte(ext))
		}
	})
}

func emojiPB(u []byte, content string) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(emojiCommon, commonPB(1700000011))
		w.Bytes(emojiUser, u)
		w.Bytes(emojiContent, []byte(content))
	})
}

func streamPB(action uint64) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(streamCommon, commonPB(1700000012))
		if action != 0 {
			w.UInt64(streamAction, action)
		}
	})
}

func messagePB(method string, payload []byte) []byte {
	return pb(func(w *protoio.Writer) {
		w.Bytes(msgMethod, []byte(method))
		w.Bytes(msgPayload, payload)
	})
}

func responsePB(messages [][]byte, cursor string, now uint64, internalExt string) []byte {
	return pb(func(w *protoio.Writer) {
		for _, m := range messages {
			w.Bytes(respMessages, m)
		}
		if cursor != "" {
			w.Bytes(respCursor, []byte(cursor))
		}
		if now != 0 {
			w.UInt64(respNow, now)
		}
		if internalExt != "" {
			w.Bytes(respInternalExt, []byte(internalExt))
		}
	})
}

// ---- shared fixtures ----

const roomIDFixture = "76600000000123"

// errExpectedDrop marks server-side session closures that the test plans for.
var errExpectedDrop = errors.New("expected test drop")

// pageFixture models the room page HTML carrying the roomId JSON string that
// extractRoomID locates via the douyin-meta binding field "room_id".
const pageFixture = `<html><head><script>
window.__INITIAL_STATE__={"roomId":"` + roomIDFixture + `","room":{"title":"测试"}};
</script></head><body>live page</body></html>`

func newPageServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, pageFixture)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// envFor redirects the room page and websocket endpoints to local servers
// (test-only overrides, mirroring MEDIAMON_INSECURE_TLS in internal/wsutil).
func envFor(t *testing.T, pageURL, wsURL string) {
	t.Helper()
	t.Setenv("MEDIAMON_LIVE_PAGE_ENDPOINT", pageURL)
	t.Setenv("MEDIAMON_LIVE_WSS_ENDPOINT", wsURL)
}

// testRegistry loads the real contract set from the repo adapt tree (same
// resolution as internal/adapt/adapt_test.go, one level deeper).
func testRegistry(t *testing.T) *contracts.Registry {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	reg := contracts.NewRegistry()
	if err := contracts.LoadDir(reg, filepath.Join(root, "adapt", "contracts")); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get(metaContractName); !ok {
		t.Fatalf("contract %q not loaded", metaContractName)
	}
	return reg
}

// testConnector builds a wired Config with a recording signer returning
// "test-sig".
func testConnector(t *testing.T) *Config {
	t.Helper()
	return &Config{
		HTTP:     httpclient.New(httpclient.Config{}),
		Registry: testRegistry(t),
		Signer: func(urlQuery string, params map[string]string) (string, error) {
			return "test-sig", nil
		},
		Obs: obs.NewCounterMap(),
	}
}

func collectEvents() (chan model.LiveEvent, func(ev model.LiveEvent) error) {
	ch := make(chan model.LiveEvent, 256)
	return ch, func(ev model.LiveEvent) error {
		ch <- ev
		return nil
	}
}

func drainEvents(ch chan model.LiveEvent) []model.LiveEvent {
	var out []model.LiveEvent
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}
