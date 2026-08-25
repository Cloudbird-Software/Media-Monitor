// Package protoio implements the protobuf wire format (encode/decode of
// varints, tags, and length-delimited fields) with zero dependencies.
// Intended for the message envelopes of live-room websocket traffic; full
// schema code generation is deliberately out of scope — callers walk
// fields they need via the Reader/Writer primitives.
package protoio

// Reader decodes protobuf wire-format fields from a byte slice.
type Reader struct {
	Buf []byte
	Pos int
}

// Reset attaches a new buffer.
func (r *Reader) Reset(b []byte) { r.Buf = b; r.Pos = 0 }

// Varint reads one varint value.
func (r *Reader) Varint() (uint64, bool) {
	var v uint64
	var shift uint
	for i := 0; i < 10; i++ {
		if r.Pos >= len(r.Buf) {
			return 0, false
		}
		b := r.Buf[r.Pos]
		r.Pos++
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v, true
		}
		shift += 7
	}
	return 0, false
}

// Field returns the next tag: wire type 0 varint, 1 fixed64, 2
// length-delimited, 5 fixed32.
func (r *Reader) Field() (num int, wire int, ok bool) {
	tag, valid := r.Varint()
	if !valid {
		return 0, 0, false
	}
	return int(tag >> 3), int(tag & 7), true
}

// LenBytes reads a length-delimited payload (wire type 2).
func (r *Reader) LenBytes() ([]byte, bool) {
	n, ok := r.Varint()
	if !ok || n > uint64(len(r.Buf)-r.Pos) {
		r.Pos = len(r.Buf)
		return nil, false
	}
	b := r.Buf[r.Pos : r.Pos+int(n)]
	r.Pos += int(n)
	return b, true
}

// Skip consumes a field of the given wire type.
func (r *Reader) Skip(wire int) bool {
	switch wire {
	case 0:
		_, ok := r.Varint()
		return ok
	case 1:
		if r.Pos+8 > len(r.Buf) {
			return false
		}
		r.Pos += 8
		return true
	case 2:
		_, ok := r.LenBytes()
		return ok
	case 5:
		if r.Pos+4 > len(r.Buf) {
			return false
		}
		r.Pos += 4
		return true
	default:
		return false
	}
}

// Done reports whether the buffer is fully consumed.
func (r *Reader) Done() bool { return r.Pos >= len(r.Buf) }

// Writer encodes protobuf fields into a byte slice.
type Writer struct{ B []byte }

// Varint appends a varint value.
func (w *Writer) Varint(v uint64) {
	for v >= 0x80 {
		w.B = append(w.B, byte(v)|0x80)
		v >>= 7
	}
	w.B = append(w.B, byte(v))
}

// Tag appends a field tag.
func (w *Writer) Tag(num, wire int) { w.Varint(uint64(num)<<3 | uint64(wire)) }

// Bytes appends a length-delimited field.
func (w *Writer) Bytes(num int, b []byte) {
	w.Tag(num, 2)
	w.Varint(uint64(len(b)))
	w.B = append(w.B, b...)
}

// UInt64 appends a varint scalar field.
func (w *Writer) UInt64(num int, v uint64) {
	w.Tag(num, 0)
	w.Varint(v)
}

// BytesOut returns the accumulated buffer.
func (w *Writer) BytesOut() []byte { return w.B }
