package live

import (
	"encoding/json"

	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// XHS live method names (JSON ws channel). Evidence: the original software
// only exposes the service identifiers xiaohongshu.live.room.info /
// xiaohongshu.live.room.refresh / xiaohongshu.live.user (chunk strings); no
// xhs-specific ws method strings were found, so the method vocabulary below
// follows the *Message naming convention reconstructed from the original
// software's Webcast*/SCWeb* chunk strings, and the frame format reuses the
// kuaishou gunzip+base64 JSON shape (limited evidence).
const (
	xhsMethodComment = "CommentMessage"
	xhsMethodGift    = "GiftMessage"
	xhsMethodLike    = "LikeMessage"
	xhsMethodMember  = "MemberMessage"
	xhsMethodStats   = "RoomStatsMessage"
)

// xhsDecoder reuses the kuaishou gunzip+base64 JSON wire format (the xhs live
// channel uses the same transport shape); only the method vocabulary differs.
type xhsDecoder struct {
	kuaishouDecoder
}

// newXhsDecoder builds an xhs decoder from the xhs-meta protocol_methods table.
func newXhsDecoder(methods map[string]string) *xhsDecoder {
	return &xhsDecoder{kuaishouDecoder{methods: methods}}
}

// NewXhsDecoder exposes the xhs decoder for the CLI/MCP surface.
func NewXhsDecoder(methods map[string]string) Decoder {
	return newXhsDecoder(methods)
}

// Event maps a decoded xhs message to a LiveEvent (implements Decoder).
func (d *xhsDecoder) Event(roomID, method string, payload []byte, now int64) (model.LiveEvent, bool) {
	return EventFromXhsMessage(roomID, method, payload, now)
}

// EventFromXhsMessage maps a decoded xhs message to a LiveEvent.
func EventFromXhsMessage(roomID, method string, payload []byte, now int64) (model.LiveEvent, bool) {
	ev := model.LiveEvent{RoomID: roomID, Time: now}
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		return ev, false
	}
	switch method {
	case xhsMethodComment:
		ev.Event = "chat"
		ev.Content = stringFrom(msg["content"])
		ev.User = ksUser(msg["user"])
	case xhsMethodLike:
		ev.Event = "like"
		ev.Count = int64From(msg["count"])
		ev.User = ksUser(msg["user"])
	case xhsMethodGift:
		ev.Event = "gift"
		ev.Content = stringFrom(msg["gift_name"])
		ev.Count = int64From(msg["count"])
		ev.User = ksUser(msg["user"])
	case xhsMethodMember:
		ev.Event = "enter"
		ev.User = ksUser(msg["user"])
	case xhsMethodStats:
		ev.Event = "room_stat"
		ev.Count = int64From(msg["count"])
	default:
		return ev, false
	}
	return ev, true
}
