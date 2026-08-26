// Package live monitors douyin live rooms: it resolves the room page via the
// douyin-meta contract, opens the im-push websocket with an injected
// signature, keeps the session alive with hb/ack PushFrames, and decodes the
// webcast message families into model.LiveEvent streams. The wire layers
// (RFC 6455, protobuf wire format, gzip) are handled by internal/wsutil and
// internal/protoio; there is no generated protobuf code and no third-party
// module.
//
// Test isolation: all tests run against local httptest servers. The two
// endpoints can be redirected via the test-only environment overrides
// MEDIAMON_LIVE_PAGE_ENDPOINT (room page) and MEDIAMON_LIVE_WSS_ENDPOINT
// (websocket), mirroring the MEDIAMON_INSECURE_TLS escape hatch in
// internal/wsutil; neither is ever set in production.
package live

import (
	"encoding/binary"
	"errors"

	"github.com/Cloudbird-Software/Media-Monitor/internal/protoio"
)

// Protobuf wire types used by the walkers in this package.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireLen     = 2
	wireFixed32 = 5
)

// PushFrame is the im-push channel control frame, the only client-to-server
// payload kind (heartbeat "hb" and acknowledgement "ack"). Wire layout:
//
//	{1: bytes payload_type, 2: uint64 logid, 7: bytes payload}
//
// Frames travel length-delimited: a 4-byte big-endian length prefix followed
// by the protobuf payload (encodeDelimited framing).
const (
	pushFramePayloadType = 1
	pushFrameLogID       = 2
	pushFramePayload     = 7
)

// PushFrame is one client-to-server control frame.
type PushFrame struct {
	PayloadType string
	LogID       uint64
	Payload     []byte
}

// EncodePushFrame serializes a PushFrame and prefixes it with the 4-byte
// big-endian length (encodeDelimited framing of the im push channel).
// Omitted fields stay absent on the wire: a heartbeat is exactly {1:"hb"}.
func EncodePushFrame(f PushFrame) []byte {
	var w protoio.Writer
	w.Bytes(pushFramePayloadType, []byte(f.PayloadType))
	if f.LogID != 0 {
		w.UInt64(pushFrameLogID, f.LogID)
	}
	if len(f.Payload) > 0 {
		w.Bytes(pushFramePayload, f.Payload)
	}
	pb := w.BytesOut()
	out := make([]byte, 4+len(pb))
	binary.BigEndian.PutUint32(out, uint32(len(pb)))
	copy(out[4:], pb)
	return out
}

// DecodePushFrame reverses EncodePushFrame for one length-prefixed frame.
func DecodePushFrame(frame []byte) (PushFrame, bool) {
	if len(frame) < 4 {
		return PushFrame{}, false
	}
	n := int(binary.BigEndian.Uint32(frame))
	if n != len(frame)-4 {
		return PushFrame{}, false
	}
	var f PushFrame
	var r protoio.Reader
	r.Reset(frame[4:])
	for !r.Done() {
		num, wire, ok := r.Field()
		if !ok {
			return PushFrame{}, false
		}
		switch {
		case num == pushFramePayloadType && wire == wireLen:
			v, ok := r.LenBytes()
			if !ok {
				return PushFrame{}, false
			}
			f.PayloadType = string(v)
		case num == pushFrameLogID && wire == wireVarint:
			v, ok := r.Varint()
			if !ok {
				return PushFrame{}, false
			}
			f.LogID = v
		case num == pushFramePayload && wire == wireLen:
			v, ok := r.LenBytes()
			if !ok {
				return PushFrame{}, false
			}
			f.Payload = v
		default:
			if !r.Skip(wire) {
				return PushFrame{}, false
			}
		}
	}
	return f, true
}

// Response is the downstream envelope decoded from each websocket message
// (optionally gzip-compressed). Wire layout:
//
//	{1: messages_list (repeated Message), 2: cursor, 3: fetch_interval,
//	 4: now, 5: internal_ext, 6: fetch_type, 7: params}
const (
	respMessages      = 1
	respCursor        = 2
	respFetchInterval = 3
	respNow           = 4
	respInternalExt   = 5
	respFetchType     = 6
	respParams        = 7
)

// Response is one decoded downstream envelope.
type Response struct {
	Messages      [][]byte
	Cursor        string
	FetchInterval uint64
	Now           uint64
	InternalExt   string
	FetchType     uint64
	Params        map[string]string
}

// DecodeResponse parses a (possibly decompressed) Response envelope.
func DecodeResponse(b []byte) (Response, error) {
	var out Response
	if len(b) == 0 {
		return out, errors.New("live: empty response")
	}
	var r protoio.Reader
	r.Reset(b)
	for !r.Done() {
		num, wire, ok := r.Field()
		if !ok {
			return out, errors.New("live: response: malformed tag")
		}
		switch {
		case num == respMessages && wire == wireLen:
			v, ok := r.LenBytes()
			if !ok {
				return out, errors.New("live: response: truncated message")
			}
			out.Messages = append(out.Messages, v)
		case num == respCursor && wire == wireLen:
			v, ok := r.LenBytes()
			if !ok {
				return out, errors.New("live: response: truncated cursor")
			}
			out.Cursor = string(v)
		case num == respFetchInterval && wire == wireVarint:
			out.FetchInterval, _ = r.Varint()
		case num == respNow && wire == wireVarint:
			out.Now, _ = r.Varint()
		case num == respInternalExt && wire == wireLen:
			v, ok := r.LenBytes()
			if !ok {
				return out, errors.New("live: response: truncated internal_ext")
			}
			out.InternalExt = string(v)
		case num == respFetchType && wire == wireVarint:
			out.FetchType, _ = r.Varint()
		case num == respParams && wire == wireLen:
			v, ok := r.LenBytes()
			if !ok {
				return out, errors.New("live: response: truncated param entry")
			}
			if out.Params == nil {
				out.Params = map[string]string{}
			}
			if key, val, ok := decodeParamEntry(v); ok {
				out.Params[key] = val
			}
		default:
			if !r.Skip(wire) {
				return out, errors.New("live: response: malformed field")
			}
		}
	}
	return out, nil
}

// decodeParamEntry parses one map<string,string> entry {1: key, 2: value}.
func decodeParamEntry(b []byte) (key, value string, ok bool) {
	var r protoio.Reader
	r.Reset(b)
	for !r.Done() {
		num, wire, ok := r.Field()
		if !ok {
			return "", "", false
		}
		switch {
		case num == 1 && wire == wireLen:
			v, _ := r.LenBytes()
			key = string(v)
		case num == 2 && wire == wireLen:
			v, _ := r.LenBytes()
			value = string(v)
		default:
			if !r.Skip(wire) {
				return "", "", false
			}
		}
	}
	return key, value, true
}

// Message wraps one event payload inside a Response. Wire layout:
//
//	{1: method (string), 2: payload (bytes)}
const (
	msgMethod  = 1
	msgPayload = 2
)

// DecodeMessage splits a Message wrapper into its method name and payload.
func DecodeMessage(b []byte) (method string, payload []byte, ok bool) {
	var r protoio.Reader
	r.Reset(b)
	for !r.Done() {
		num, wire, fok := r.Field()
		if !fok {
			return "", nil, false
		}
		switch {
		case num == msgMethod && wire == wireLen:
			v, lok := r.LenBytes()
			if !lok {
				return "", nil, false
			}
			method = string(v)
		case num == msgPayload && wire == wireLen:
			v, lok := r.LenBytes()
			if !lok {
				return "", nil, false
			}
			payload = v
		default:
			if !r.Skip(wire) {
				return "", nil, false
			}
		}
	}
	return method, payload, true
}
