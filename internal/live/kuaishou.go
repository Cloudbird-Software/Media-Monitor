package live

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// Kuaishou method names (SCWeb* family, gunzip+base64 wrapped JSON).
const (
	ksMethodFeedPush = "SCWebFeedPush"
	ksMethodWatching = "SCWebLiveWatchingUsers"
	ksMethodEnterAck = "SCWebEnterRoomAck"
	ksMethodLike     = "SCWebLikeMessage"
	ksMethodGift     = "SCWebGiftMessage"
	ksMethodComment  = "SCWebCommentMessage"
)

// kuaishouDecoder decodes kuaishou's gunzip+base64-wrapped JSON downlink
// frames into Decoded messages. Evidence: SCWebFeedPush/SCWebLiveWatchingUsers
// strings + "gunzip+base64" frame format noted in docs/api-formats-douyin.md.
type kuaishouDecoder struct {
	methods map[string]string // proto method -> event key
}

// newKuaishouDecoder builds a decoder from the kuaishou-meta contract's
// protocol_methods table.
func newKuaishouDecoder(methods map[string]string) *kuaishouDecoder {
	return &kuaishouDecoder{methods: methods}
}

// NewKuaishouDecoder exposes the kuaishou decoder for the CLI/MCP surface.
func NewKuaishouDecoder(methods map[string]string) Decoder {
	return newKuaishouDecoder(methods)
}

// Decode base64-decodes the frame, gunzips if gzipped, then parses the JSON
// envelope. Kuaishou frames are either a single message object or an array.
func (d *kuaishouDecoder) Decode(frame []byte) ([]Decoded, error) {
	plain, err := d.decodeFrame(frame)
	if err != nil {
		return nil, err
	}
	// Try an array of messages first.
	var arr []map[string]any
	if err := json.Unmarshal(plain, &arr); err == nil {
		return d.mapArray(arr), nil
	}
	// Single message object.
	var single map[string]any
	if err := json.Unmarshal(plain, &single); err != nil {
		return nil, fmt.Errorf("kuaishou: frame is not JSON: %w", err)
	}
	return d.mapSingle(single), nil
}

// decodeFrame base64-decodes then gunzips (if gzipped) the raw frame.
func (d *kuaishouDecoder) decodeFrame(frame []byte) ([]byte, error) {
	// base64 unwrap (kuaishou wraps the gzip payload in base64).
	b64 := bytes.TrimRight(frame, "\x00")
	decoded, err := base64.StdEncoding.DecodeString(string(b64))
	if err != nil {
		// Some frames ship raw gzip without base64; fall through to gunzip.
		decoded = frame
	}
	// gunzip if gzipped.
	if len(decoded) >= 2 && decoded[0] == 0x1f && decoded[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(decoded))
		if err != nil {
			return nil, fmt.Errorf("kuaishou: gzip: %w", err)
		}
		defer zr.Close()
		out, err := io.ReadAll(zr)
		if err != nil {
			return nil, fmt.Errorf("kuaishou: gzip read: %w", err)
		}
		return out, nil
	}
	return decoded, nil
}

func (d *kuaishouDecoder) mapArray(arr []map[string]any) []Decoded {
	out := make([]Decoded, 0, len(arr))
	for _, m := range arr {
		out = append(out, d.convert(m))
	}
	return out
}

func (d *kuaishouDecoder) mapSingle(m map[string]any) []Decoded {
	return []Decoded{d.convert(m)}
}

// convert maps one JSON message to a Decoded. The method name lives under
// "method"/"msg_type"/"type"; the payload is the whole object re-encoded.
func (d *kuaishouDecoder) convert(m map[string]any) Decoded {
	method := stringFrom(m["method"])
	if method == "" {
		method = stringFrom(m["msg_type"])
	}
	if method == "" {
		method = stringFrom(m["type"])
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return Decoded{OK: false}
	}
	return Decoded{Method: method, Payload: payload, OK: method != ""}
}

func stringFrom(v any) string {
	s, _ := v.(string)
	return s
}

// AckToken: kuaishou uses no internal_ext-style ack token.
func (d *kuaishouDecoder) AckToken() string { return "" }

// Heartbeat: kuaishou hb is a JSON ping.
func (d *kuaishouDecoder) Heartbeat() []byte {
	b, _ := json.Marshal(map[string]any{"type": "heartbeat"})
	return b
}

// Event maps a decoded kuaishou message to a LiveEvent (implements Decoder).
func (d *kuaishouDecoder) Event(roomID, method string, payload []byte, now int64) (model.LiveEvent, bool) {
	return EventFromKuaishouMessage(roomID, method, payload, now)
}

// EventFromKuaishouMessage maps a decoded kuaishou message to a LiveEvent,
// reusing the same event keys as the douyin surface where they align.
func EventFromKuaishouMessage(roomID, method string, payload []byte, now int64) (model.LiveEvent, bool) {
	ev := model.LiveEvent{RoomID: roomID, Time: now}
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		return ev, false
	}
	switch method {
	case ksMethodComment, ksMethodFeedPush:
		ev.Event = "chat"
		ev.Content = stringFrom(msg["content"])
		ev.User = ksUser(msg["user"])
	case ksMethodLike:
		ev.Event = "like"
		ev.Count = int64From(msg["count"])
		ev.User = ksUser(msg["user"])
	case ksMethodGift:
		ev.Event = "gift"
		ev.Content = stringFrom(msg["gift_name"])
		ev.Count = int64From(msg["count"])
		ev.User = ksUser(msg["user"])
	case ksMethodEnterAck:
		ev.Event = "enter"
	case ksMethodWatching:
		ev.Event = "room_stat"
		ev.Count = int64From(msg["count"])
	default:
		return ev, false
	}
	return ev, true
}

func ksUser(v any) model.UserProfile {
	var u model.UserProfile
	if m, ok := v.(map[string]any); ok {
		u.UID = stringFrom(m["uid"])
		u.Nickname = stringFrom(m["nickname"])
		u.AvatarURL = stringFrom(m["avatar"])
	}
	return u
}

func int64From(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		var i int64
		fmt.Sscanf(n, "%d", &i)
		return i
	}
	return 0
}

var _ = context.Background
var _ = errors.New
