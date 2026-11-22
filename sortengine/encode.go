package sortengine

import (
	"encoding/binary"
	"math"
)

// Encoding layout for one row's key:
//
//	[column 0 bytes][column 1 bytes]...[column n-1 bytes]
//
// Every column is encoded so that a plain byte-wise comparison of two keys
// yields exactly the multi-column ORDER BY result. Within one column the
// layout is
//
//	null marker (1 byte)
//	value bytes
//
// The null marker comes first so that NULL placement never has to inspect the
// value. DESC is implemented by bitwise-inverting the value bytes only; the
// null marker is placed at the head or tail by choosing its bit, so the
// high-bit-first radix pass on the first byte also partitions NULLs correctly.
//
// Fixed-width value columns (bool/int/uint/float) always emit the same byte
// count, so no terminator is needed. Variable-width string columns are
// self-delimiting:
//
//	UTF-8 bytes, with a 0x00 byte written as 0x00 0x01, terminated by 0x00 0x00
//
// The 0x01 escape after every 0x00 keeps the encoding order-preserving even
// when one string is a prefix of another: "a" -> 61 00 00, "a\x00" -> 61 00 01
// 00 00, and "aa" -> 61 61 00 00. Comparing byte 3 decides in favour of "a"
// (0x00 < 0x61), and "a\x00" compares below "aa" because 0x01 < 0x61, which is
// what raw UTF-8 comparison says too. No UTF-8 byte is ever 0x00, so the
// escape is unambiguous.
//
// Pinyin string columns replace each rune with a two- or three-byte ordering
// unit (see appendPinyinString) followed by a zero-word terminator. Because no
// unit is an all-zero word, the escape/terminator scheme is unnecessary: byte
// comparison of code sequences
// is the lexicographic order of the reading sequences, and a shorter reading
// sequence (a prefix) sorts first, matching dictionary order.
//
// Mixed-kind (KindAny) columns prefix a one-byte type tag before the value
// bytes. The tag is inside the DESC inversion along with the value so that a
// homogeneous Any column still reverses correctly; the null marker stays
// outside.

const (
	nullFirstMark  byte = 0x00 // NULL sorts first: marker 0x00 < non-null 0x01
	nullLastMark   byte = 0xff // NULL sorts last: marker 0xff > non-null 0xfe
	nonNullFirst   byte = 0x01
	nonNullLast    byte = 0xfe
	anyKindTagBase byte = 1 // tags 1..5 for bool,int,uint,float,string
)

// wideColumn reports whether a column has a fixed-width encoding.
func wideColumn(k Kind) bool {
	switch k {
	case KindBool, KindInt, KindUint, KindFloat:
		return true
	}
	return false
}

// KeyWidth returns the encoded width of a fixed-width column in bytes, or 0 if
// the column is variable-width.
func KeyWidth(k Kind) int {
	switch k {
	case KindBool:
		return 1 + 1
	case KindInt, KindUint, KindFloat:
		return 1 + 8
	}
	return 0
}

// encBuf is a growable byte buffer reused across rows to avoid per-row
// allocations. Each row's key is copied out of it once complete.
type encBuf struct {
	b []byte
	// offsets[i] is where column i's bytes start; offsets[len(cols)] is the
	// end. Used to render the human-readable column breakdown.
	offsets []int
}

// AppendKey encodes one row's sort key into dst and returns it. The result is
// valid until dst is reused; Sort always copies it per row.
func (s *Sorter) AppendKey(dst []byte, row Row) []byte {
	dst = dst[:0]
	for _, c := range s.cols {
		v := c.expr.Eval(row)
		dst = appendColumn(dst, c.col, v)
	}
	return dst
}

// appendKeyWithOffsets is AppendKey that also records per-column byte offsets.
func (s *Sorter) appendKeyWithOffsets(buf *encBuf, row Row) []byte {
	buf.b = buf.b[:0]
	if cap(buf.offsets) < len(s.cols)+1 {
		buf.offsets = make([]int, len(s.cols)+1)
	}
	buf.offsets = buf.offsets[:len(s.cols)+1]
	for i, c := range s.cols {
		buf.offsets[i] = len(buf.b)
		v := c.expr.Eval(row)
		buf.b = appendColumn(buf.b, c.col, v)
	}
	buf.offsets[len(s.cols)] = len(buf.b)
	return buf.b
}

func appendColumn(dst []byte, col Column, v Value) []byte {
	// Coerce to the declared type, then handle NULL. NULL placement is decided
	// by one marker byte at the head of the column, independent of the value
	// encoding.
	v = Coerce(v, col.Kind)
	if v.Kind == KindNull {
		if col.Nulls == NullsLast {
			return append(dst, nullLastMark)
		}
		return append(dst, nullFirstMark)
	}
	var mark byte
	if col.Nulls == NullsLast {
		mark = nonNullLast
	} else {
		mark = nonNullFirst
	}
	dst = append(dst, mark)
	return appendRawValue(dst, col.Kind, col.Pinyin, col.Dir == Desc, v)
}

// appendRawValue appends one non-NULL value's order-bearing bytes (no null
// marker). desc inverts exactly these bytes.
func appendRawValue(dst []byte, kind Kind, pinyin, desc bool, v Value) []byte {
	switch kind {
	case KindBool:
		var b byte
		if v.Bool {
			b = 1
		}
		if desc {
			b = ^b
		}
		return append(dst, b)
	case KindInt:
		u := uint64(v.Int) ^ signBit
		if desc {
			u = ^u
		}
		return binary.BigEndian.AppendUint64(dst, u)
	case KindUint:
		u := v.Uint
		if desc {
			u = ^u
		}
		return binary.BigEndian.AppendUint64(dst, u)
	case KindFloat:
		u := orderFloat(math.Float64bits(NormalizeFloat(math.Float64frombits(v.Float))))
		if desc {
			u = ^u
		}
		return binary.BigEndian.AppendUint64(dst, u)
	case KindString:
		if pinyin {
			return appendPinyinString(dst, v.Str, desc)
		}
		return appendEscapedString(dst, v.Str, desc)
	case KindAny:
		return appendAny(dst, pinyin, desc, v)
	}
	return dst
}

// appendEscapedString implements the 0x00 -> 0x00 0x01 escape with a 0x00 0x00
// terminator. With desc set, all emitted value bytes are inverted; inversion is
// its own inverse, so the terminator becomes ff ff and the escape ff fe, which
// preserves the order while reversing it.
func appendEscapedString(dst []byte, s string, desc bool) []byte {
	if !desc {
		for i := 0; i < len(s); i++ {
			c := s[i]
			dst = append(dst, c)
			if c == 0 {
				dst = append(dst, 1)
			}
		}
		return append(dst, 0, 0)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		dst = append(dst, ^c)
		if c == 0 {
			dst = append(dst, ^byte(1))
		}
	}
	return append(dst, 0xff, 0xff)
}

// Pinyin code bands. A reading is syllable*5+tone in [1, 2094], encoded as a
// two-byte unit whose first byte is in [0x00, 0x08]. Bytes without a reading
// are encoded as a three-byte unit starting at 0xC0 | (r >> 16), i.e. in
// [0xC0, 0xD0], which orders them after every reading and among themselves by
// code point. The all-zero word is the terminator, and 0x00 0x00 can never be
// a unit: code 0 does not exist and the no-reading band starts at 0xC0.
//
// The units are prefix-free at the first byte, so the byte string of a code
// sequence compares lexicographically like the code sequence itself, and a
// sequence that is a prefix of another sorts first. Under DESC every emitted
// byte is inverted, including the terminator (0xff 0xff 0xff), which reverses
// both rules consistently.
func appendPinyinString(dst []byte, s string, desc bool) []byte {
	if !desc {
		for _, r := range s {
			if code, ok := pinyinLookup(r); ok {
				dst = append(dst, byte(code>>8), byte(code))
			} else {
				r32 := uint32(r)
				dst = append(dst, byte(0xC0+(r32>>16)&0x1f), byte(r32>>8), byte(r32))
			}
		}
		return append(dst, 0, 0, 0)
	}
	for _, r := range s {
		if code, ok := pinyinLookup(r); ok {
			dst = append(dst, ^byte(code>>8), ^byte(code))
		} else {
			r32 := uint32(r)
			dst = append(dst, ^byte(0xC0+(r32>>16)&0x1f), ^byte(r32>>8), ^byte(r32))
		}
	}
	return append(dst, 0xff, 0xff, 0xff)
}

// appendAny prefixes the value with a kind tag. The tag orders families
// bool < int < uint < float < string; within one family the value bytes decide.
// The tag is inverted together with the value under DESC.
func appendAny(dst []byte, pinyin, desc bool, v Value) []byte {
	var tag byte
	switch v.Kind {
	case KindBool:
		tag = anyKindTagBase
	case KindInt:
		tag = anyKindTagBase + 1
	case KindUint:
		tag = anyKindTagBase + 2
	case KindFloat:
		tag = anyKindTagBase + 3
	case KindString:
		tag = anyKindTagBase + 4
	default:
		return append(dst, 0)
	}
	if desc {
		tag = ^tag
	}
	dst = append(dst, tag)
	return appendRawValue(dst, v.Kind, pinyin, desc, v)
}

// compareEncoded compares two encoded keys in one shot. It is exactly
// bytes.Compare; it exists so the encoder's contract is exercised in tests
// against the same function the radix sort's fallback uses.
func compareEncoded(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// KeyHex renders an encoded key as lowercase hex for the demo page.
func KeyHex(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[2*i] = hexdigits[c>>4]
		out[2*i+1] = hexdigits[c&0xf]
	}
	return string(out)
}
