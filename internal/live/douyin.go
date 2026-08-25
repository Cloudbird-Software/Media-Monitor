package live

import (
	"strconv"

	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/protoio"
)

// Webcast message family method names (douyin webcast im channel).
const (
	methodChat                 = "WebcastChatMessage"
	methodGift                 = "WebcastGiftMessage"
	methodLike                 = "WebcastLikeMessage"
	methodMember               = "WebcastMemberMessage"
	methodSocial               = "WebcastSocialMessage"
	methodRoomUserSeq          = "WebcastRoomUserSeqMessage"
	methodFansclub             = "WebcastFansclubMessage"
	methodRoomRank             = "WebcastRoomRankMessage"
	methodRoomStats            = "WebcastRoomStatsMessage"
	methodControl              = "WebcastControlMessage"
	methodEmojiChat            = "WebcastEmojiChatMessage"
	methodRoomStreamAdaptation = "WebcastRoomStreamAdaptationMessage"
)

// defaultProtocolMethods is the built-in fallback table (proto method name
// -> event key); the douyin-meta contract protocol_methods value overrides
// or extends it when present.
func defaultProtocolMethods() map[string]string {
	return map[string]string{
		methodChat:                 "chat",
		methodGift:                 "gift",
		methodLike:                 "like",
		methodMember:               "enter",
		methodSocial:               "follow",
		methodRoomUserSeq:          "seq",
		methodFansclub:             "fansclub",
		methodRoomRank:             "rank",
		methodRoomStats:            "room_stat",
		methodControl:              "control",
		methodEmojiChat:            "emoji",
		methodRoomStreamAdaptation: "stream",
	}
}

// Field numbers below are the runtime locators of the public douyin webcast
// im messages (the layout mirrored by open-source protocol dumps of the
// version_code=180800 channel). They are deliberately not contract-declared
// bindings: the channel evolves constantly and the walkers below skip unknown
// fields leniently. The fixtures in live_test.go encode the same numbers via
// internal/protoio.
//
// Common envelope of every event payload (field 1, nested message).
const (
	commonFieldNumber = 1 // nested Common message (field 1 of event payload)
	commonCreateTime  = 4 // uint64, unix seconds (inside Common)
)

// User profile (nested message at various family field numbers).
const (
	userID        = 1  // uint64 uid
	userShortID   = 2  // uint64 dy short id
	userNickname  = 3  // string
	userGender    = 4  // uint32: 0 unknown, 1 male, 2 female
	userSignature = 5  // string
	userAvatar    = 9  // repeated Img{ url_list = 1 }
	userSecUID    = 46 // string, MS4... form
	userIPLabel   = 60 // string (best-effort on newer channels)
	imgURLList    = 1  // repeated string inside Img
)

// Family-specific layouts (per-family nested Common/User as above).
const (
	chatCommon  = 1
	chatUser    = 2
	chatContent = 3 // string

	giftCommon      = 1
	giftID          = 2 // uint64
	giftCount       = 3 // uint64 batch count
	giftName        = 4 // string
	giftUser        = 7 // User, the giver
	giftRepeatCount = 9 // uint64 combo count

	likeCommon = 1
	likeCount  = 2 // uint64 per-message like count
	likeTotal  = 3 // uint64 cumulative likes
	likeUser   = 5 // User

	memberCommon = 1
	memberUser   = 2
	memberCount  = 3 // uint64
	memberAction = 4 // string

	socialCommon = 1
	socialUser   = 2

	seqTotal = 3 // uint64 online total (follow+like system raw total)

	fansclubCommon  = 1
	fansclubAction  = 2 // string
	fansclubContent = 3 // string
	fansclubUser    = 6 // User

	rankItems      = 2 // repeated RankItem
	rankTotalUsers = 3 // uint64
	rankItemUserID = 1 // uint64
	rankItemUser   = 2 // string user name
	rankItemRank   = 3 // string
	rankItemScore  = 4 // string

	roomStatCommon = 1
	roomStatViewer = 2 // uint64 watcher count

	controlCommon = 1
	controlStatus = 2 // uint32; 3 = stream ended
	controlExt    = 3 // string

	emojiCommon  = 1
	emojiUser    = 2
	emojiContent = 3 // string emoji text

	streamCommon = 1
	streamAction = 2 // uint64 (best-effort)
)

// EventFromMessage decodes one webcast message payload into a model.LiveEvent
// for roomID. recognized reports whether method is a known family (callers
// skip unknown methods silently — the channel constantly adds new ones).
// controlStatus is nonzero only for WebcastControlMessage payloads where 3
// signals the live stream ended. fallbackTime seeds LiveEvent.Time when the
// payload carries no create_time.
func EventFromMessage(roomID, method string, payload []byte, fallbackTime int64) (ev model.LiveEvent, controlStatus int64, recognized bool) {
	ev = model.LiveEvent{
		RoomID:     roomID,
		Time:       commonTime(payload, fallbackTime),
		RawSummary: map[string]any{},
	}
	switch method {
	case methodChat:
		ev.Event = "chat"
		decodeChat(payload, &ev)
	case methodGift:
		ev.Event = "gift"
		decodeGift(payload, &ev)
	case methodLike:
		ev.Event = "like"
		decodeLike(payload, &ev)
	case methodMember:
		ev.Event = "enter"
		decodeUser(payload, &ev, memberUser)
	case methodSocial:
		ev.Event = "follow"
		decodeUser(payload, &ev, socialUser)
	case methodRoomUserSeq:
		ev.Event = "seq"
		decodeSeq(payload, &ev)
	case methodFansclub:
		ev.Event = "fansclub"
		decodeFansclub(payload, &ev)
	case methodRoomRank:
		ev.Event = "rank"
		decodeRank(payload, &ev)
	case methodRoomStats:
		ev.Event = "room_stat"
		decodeRoomStat(payload, &ev)
	case methodControl:
		ev.Event = "control"
		controlStatus = decodeControl(payload, &ev)
	case methodEmojiChat:
		ev.Event = "emoji"
		decodeUser(payload, &ev, emojiUser)
		walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
			if num == emojiContent && wire == wireLen {
				if b, ok := r.LenBytes(); ok {
					ev.Content = string(b)
				}
				return true
			}
			return r.Skip(wire)
		})
	case methodRoomStreamAdaptation:
		ev.Event = "stream"
		walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
			if num == streamAction && wire == wireVarint {
				if v, ok := r.Varint(); ok {
					ev.RawSummary["action"] = v
				}
				return true
			}
			return r.Skip(wire)
		})
	default:
		return model.LiveEvent{}, 0, false
	}
	return ev, controlStatus, true
}

// walkFields iterates the protobuf fields of payload in order; fn must
// consume exactly one field's value (or skip it via r.Skip) and return
// whether the walk should continue. Malformed input ends the walk.
func walkFields(payload []byte, fn func(num, wire int, r *protoio.Reader) bool) {
	var r protoio.Reader
	r.Reset(payload)
	for !r.Done() {
		num, wire, ok := r.Field()
		if !ok || !fn(num, wire, &r) {
			return
		}
	}
}

// commonTime extracts the family's common.create_time (unix seconds) from
// the nested Common message (top-level field 1, create_time = its inner
// field 4), falling back to fallbackTime when absent/unparsable.
func commonTime(payload []byte, fallbackTime int64) int64 {
	innerCommon := func(raw []byte) []byte {
		var out []byte
		walkFields(raw, func(num, wire int, r *protoio.Reader) bool {
			if num == commonFieldNumber && wire == wireLen {
				if b, ok := r.LenBytes(); ok {
					out = b
				}
				return true
			}
			return r.Skip(wire)
		})
		return out
	}
	var t int64
	common := innerCommon(payload)
	if len(common) > 0 {
		walkFields(common, func(num, wire int, r *protoio.Reader) bool {
			if num == commonCreateTime && wire == wireVarint {
				if v, ok := r.Varint(); ok && v != 0 {
					t = int64(v)
				}
				return true
			}
			return r.Skip(wire)
		})
		if t != 0 {
			return t
		}
	}
	return fallbackTime
}

func decodeChat(payload []byte, ev *model.LiveEvent) {
	walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
		switch {
		case num == chatUser && wire == wireLen:
			if b, ok := r.LenBytes(); ok {
				ev.User = parseUser(b)
			}
		case num == chatContent && wire == wireLen:
			if b, ok := r.LenBytes(); ok {
				ev.Content = string(b)
			}
		default:
			if !r.Skip(wire) {
				return false
			}
		}
		return true
	})
}

func decodeGift(payload []byte, ev *model.LiveEvent) {
	walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
		switch {
		case num == giftID && wire == wireVarint:
			if v, ok := r.Varint(); ok {
				ev.RawSummary["gift_id"] = v
			}
		case num == giftCount && wire == wireVarint:
			if v, ok := r.Varint(); ok {
				ev.RawSummary["gift_count"] = v
			}
		case num == giftName && wire == wireLen:
			if b, ok := r.LenBytes(); ok {
				ev.Content = string(b)
			}
		case num == giftUser && wire == wireLen:
			if b, ok := r.LenBytes(); ok {
				ev.User = parseUser(b)
			}
		case num == giftRepeatCount && wire == wireVarint:
			if v, ok := r.Varint(); ok {
				ev.Count = int64(v)
				ev.RawSummary["repeat_count"] = v
			}
		default:
			if !r.Skip(wire) {
				return false
			}
		}
		return true
	})
}

func decodeLike(payload []byte, ev *model.LiveEvent) {
	walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
		switch {
		case num == likeCount && wire == wireVarint:
			if v, ok := r.Varint(); ok {
				ev.Count = int64(v)
			}
		case num == likeTotal && wire == wireVarint:
			if v, ok := r.Varint(); ok {
				ev.RawSummary["total"] = v
			}
		case num == likeUser && wire == wireLen:
			if b, ok := r.LenBytes(); ok {
				ev.User = parseUser(b)
			}
		default:
			if !r.Skip(wire) {
				return false
			}
		}
		return true
	})
}

// decodeUser stores the User message found at userNum (enter/follow/emoji).
func decodeUser(payload []byte, ev *model.LiveEvent, userNum int) {
	walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
		if num == userNum && wire == wireLen {
			if b, ok := r.LenBytes(); ok {
				ev.User = parseUser(b)
			}
			return true
		}
		return r.Skip(wire)
	})
}

func decodeSeq(payload []byte, ev *model.LiveEvent) {
	walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
		if num == seqTotal && wire == wireVarint {
			if v, ok := r.Varint(); ok {
				ev.Count = int64(v)
			}
			return true
		}
		return r.Skip(wire)
	})
}

func decodeFansclub(payload []byte, ev *model.LiveEvent) {
	walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
		switch {
		case num == fansclubContent && wire == wireLen:
			if b, ok := r.LenBytes(); ok {
				ev.Content = string(b)
			}
		case num == fansclubUser && wire == wireLen:
			if b, ok := r.LenBytes(); ok {
				ev.User = parseUser(b)
			}
		default:
			if !r.Skip(wire) {
				return false
			}
		}
		return true
	})
}

func decodeRank(payload []byte, ev *model.LiveEvent) {
	walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
		switch {
		case num == rankItems && wire == wireLen:
			b, ok := r.LenBytes()
			if !ok {
				return false
			}
			item := map[string]any{}
			walkFields(b, func(n, w int, rr *protoio.Reader) bool {
				switch {
				case n == rankItemUserID && w == wireVarint:
					if v, ok := rr.Varint(); ok {
						item["user_id"] = v
					}
				case n == rankItemUser && w == wireLen:
					if s, ok := rr.LenBytes(); ok {
						item["user_name"] = string(s)
					}
				case n == rankItemRank && w == wireLen:
					if s, ok := rr.LenBytes(); ok {
						item["rank"] = string(s)
					}
				case n == rankItemScore && w == wireLen:
					if s, ok := rr.LenBytes(); ok {
						item["score"] = string(s)
					}
				default:
					if !rr.Skip(w) {
						return false
					}
				}
				return true
			})
			ranks, _ := ev.RawSummary["ranks"].([]any)
			ev.RawSummary["ranks"] = append(ranks, item)
		case num == rankTotalUsers && wire == wireVarint:
			if v, ok := r.Varint(); ok {
				ev.RawSummary["total_user_count"] = v
			}
		default:
			if !r.Skip(wire) {
				return false
			}
		}
		return true
	})
}

func decodeRoomStat(payload []byte, ev *model.LiveEvent) {
	walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
		if num == roomStatViewer && wire == wireVarint {
			if v, ok := r.Varint(); ok {
				ev.Count = int64(v)
			}
			return true
		}
		return r.Skip(wire)
	})
}

func decodeControl(payload []byte, ev *model.LiveEvent) int64 {
	var status int64
	walkFields(payload, func(num, wire int, r *protoio.Reader) bool {
		switch {
		case num == controlStatus && wire == wireVarint:
			if v, ok := r.Varint(); ok {
				status = int64(v)
			}
		case num == controlExt && wire == wireLen:
			if b, ok := r.LenBytes(); ok {
				ev.Content = string(b)
			}
		default:
			if !r.Skip(wire) {
				return false
			}
		}
		return true
	})
	if status != 0 {
		ev.RawSummary["status"] = status
	}
	return status
}

// parseUser maps the webcast User message onto model.UserProfile.
func parseUser(b []byte) model.UserProfile {
	var u model.UserProfile
	var r protoio.Reader
	r.Reset(b)
	for !r.Done() {
		num, wire, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case num == userID && wire == wireVarint:
			if v, ok := r.Varint(); ok {
				u.UID = strconv.FormatUint(v, 10)
			}
		case num == userShortID && wire == wireVarint:
			if v, ok := r.Varint(); ok {
				u.ShortID = strconv.FormatUint(v, 10)
			}
		case num == userNickname && wire == wireLen:
			if s, ok := r.LenBytes(); ok {
				u.Nickname = string(s)
			}
		case num == userGender && wire == wireVarint:
			if v, ok := r.Varint(); ok {
				u.Gender = int(v)
			}
		case num == userSignature && wire == wireLen:
			if s, ok := r.LenBytes(); ok {
				u.Signature = string(s)
			}
		case num == userAvatar && wire == wireLen:
			if s, ok := r.LenBytes(); ok {
				u.AvatarURL = parseAvatarURL(s)
			}
		case num == userSecUID && wire == wireLen:
			if s, ok := r.LenBytes(); ok {
				u.SecUID = string(s)
			}
		case num == userIPLabel && wire == wireLen:
			if s, ok := r.LenBytes(); ok {
				u.IPLabel = string(s)
			}
		default:
			if !r.Skip(wire) {
				break
			}
		}
	}
	return u
}

// parseAvatarURL returns the first url_list entry of the first Img item.
func parseAvatarURL(b []byte) string {
	var r protoio.Reader
	r.Reset(b)
	for !r.Done() {
		num, wire, ok := r.Field()
		if !ok {
			break
		}
		if num == imgURLList && wire == wireLen {
			if s, ok := r.LenBytes(); ok && len(s) > 0 {
				return string(s)
			}
			continue
		}
		if !r.Skip(wire) {
			break
		}
	}
	return ""
}
