package live

import "github.com/Cloudbird-Software/Media-Monitor/internal/model"

// Decoder is the platform-agnostic live-wire abstraction. The douyin channel
// speaks protobuf (DecodeResponse/DecodeMessage); kuaishou/xhs speak
// gunzip+base64-wrapped JSON (SCWebFeedPush/...). Each platform ships one
// Decoder implementation; runSession picks the right one from the contract
// category (live_meta) and platform.
//
// A Decoder turns one decompressed downlink frame into zero or more decoded
// messages; the session loop acks, maps method->event and delivers.
type Decoder interface {
	// Decode turns one decompressed frame into decoded messages. A message
	// with Method=="" and OK=false is silently skipped by the session loop.
	Decode(frame []byte) ([]Decoded, error)
	// AckToken, if non-empty, is echoed as the ack payload for the current
	// batch (the douyin internal_ext equivalent). "" means no ack.
	AckToken() string
	// Heartbeat returns the bytes to send as a heartbeat PushFrame.
	Heartbeat() []byte
	// Event maps a decoded message to a LiveEvent. The per-decoder method is
	// how kuaishou/xhs JSON events get normalized without forking the loop.
	Event(roomID, method string, payload []byte, now int64) (model.LiveEvent, bool)
}

// Decoded is one decoded downlink message.
type Decoded struct {
	Method  string // platform method name (e.g. WebcastChatMessage / SCWebFeedPush)
	Payload []byte // raw payload for EventFromMessage
	OK      bool   // false = unrecognized/undecodeable (skipped)
}
