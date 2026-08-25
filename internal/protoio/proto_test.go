package protoio

import (
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

func TestRoundTripFixedFragment(t *testing.T) {
	var w Writer
	w.UInt64(1, 150)
	w.Bytes(2, []byte("test"))
	w.Varint(3)
	w.Bytes(3, []byte{0x08, 0x96, 0x01})

	var r Reader
	r.Reset(w.BytesOut())
	type field struct {
		num, wire int
		vi        uint64
		by        []byte
	}
	var got []field
	for !r.Done() {
		num, wire, ok := r.Field()
		if !ok {
			t.Fatalf("Field failed at pos %d", r.Pos)
		}
		switch wire {
		case 0:
			v, _ := r.Varint()
			got = append(got, field{num, wire, v, nil})
		case 2:
			b, _ := r.LenBytes()
			got = append(got, field{num, wire, 0, b})
		}
	}
	if len(got) != 3 {
		t.Fatalf("want 3 fields, got %d: %+v", len(got), got)
	}
	if got[0].num != 1 || got[0].vi != 150 {
		t.Errorf("field1 = %+v", got[0])
	}
	if got[1].num != 2 || string(got[1].by) != "test" {
		t.Errorf("field2 = %+v", got[1])
	}
	if string(got[2].by) != string([]byte{0x08, 0x96, 0x01}) {
		t.Errorf("field3 = %+v", got[2])
	}
}

func TestSkipAndTruncation(t *testing.T) {
	var w Writer
	w.UInt64(1, 1)
	w.Bytes(2, []byte("payload"))
	raw := w.BytesOut()

	var r Reader
	r.Reset(raw)
	num, wire, _ := r.Field()
	if num != 1 || wire != 0 || !r.Skip(wire) {
		t.Fatalf("skip varint failed")
	}
	num, wire, _ = r.Field()
	if num != 2 || wire != 2 {
		t.Fatalf("second tag = %d/%d", num, wire)
	}
	b, ok := r.LenBytes()
	if !ok || string(b) != "payload" {
		t.Fatalf("lenbytes = %q ok=%v", b, ok)
	}

	// truncated length-delimited data must fail closed
	var bad Reader
	bad.Reset([]byte{0x0a, 0x7f}) // says 127 bytes, has 1
	if _, ok := bad.LenBytes(); ok {
		t.Fatal("truncated LenBytes must return false")
	}
}

func TestPropertyVarintRoundTrip(t *testing.T) {
	testkit.Run(t, 20260826, 2000, []testkit.Prop{{
		Name: "varint round-trip",
		Inv: func(r *testkit.R) string {
			v := uint64(r.R.Int63()) | uint64(r.R.Int63())<<33
			var w Writer
			w.Varint(v)
			var rd Reader
			rd.Reset(w.BytesOut())
			got, ok := rd.Varint()
			if !ok {
				return "varint decode failed"
			}
			if got != v {
				return "value mismatch"
			}
			return ""
		},
	}})
}

func TestPropertyFramedBytesRoundTrip(t *testing.T) {
	testkit.Run(t, 20260826, 1000, []testkit.Prop{{
		Name: "framed bytes round-trip",
		Inv: func(r *testkit.R) string {
			payload := r.Bytes(64)
			var w Writer
			w.Bytes(7, payload)
			var rd Reader
			rd.Reset(w.BytesOut())
			num, wire, ok := rd.Field()
			if !ok || num != 7 || wire != 2 {
				return "bad tag"
			}
			b, ok := rd.LenBytes()
			if !ok {
				return "decode failed"
			}
			if string(b) != string(payload) {
				return "payload mismatch"
			}
			return ""
		},
	}})
}
