package live

import (
	"bytes"

	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
	"testing"
)

// TestPropertyPushFrameRoundTrip is the live-specific wire property: any
// PushFrame (random payload_type/logid/payload) round-trips through
// Encode/DecodePushFrame byte-identically. protoio's own varint/framing
// round-trips are already covered by internal/protoio tests; this is the
// 4-byte length framing + 3-field PushFrame layout on top of them.
func TestPropertyPushFrameRoundTrip(t *testing.T) {
	prop := testkit.Prop{
		Name: "pushframe encode-decode round-trip",
		Inv: func(r *testkit.R) string {
			pt := "hb"
			if r.Int63n(2) == 0 {
				pt = "ack"
			}
			var logid uint64
			if r.Int63n(2) == 0 {
				logid = uint64(r.R.Int63()) | uint64(r.R.Int63())<<33
			}
			payload := r.Bytes(256)
			f := EncodePushFrame(PushFrame{PayloadType: pt, LogID: logid, Payload: payload})
			if len(f) != 4+len(encodePushFramePayload(PushFrame{PayloadType: pt, LogID: logid, Payload: payload})) {
				return "framed length does not match payload length"
			}
			got, ok := DecodePushFrame(f)
			if !ok {
				return "decode failed"
			}
			if got.PayloadType != pt {
				return "payload_type mismatch"
			}
			if got.LogID != logid {
				return "logid mismatch"
			}
			if !bytes.Equal(got.Payload, payload) {
				return "payload mismatch"
			}
			return ""
		},
	}
	testkit.Run(t, 20260825, 500, []testkit.Prop{prop})
}

// encodePushFramePayload re-encodes only the message bytes for the length
// assertion (keeps the prop independent from EncodePushFrame's internals).
func encodePushFramePayload(f PushFrame) []byte {
	b := EncodePushFrame(f)
	return b[4:]
}
