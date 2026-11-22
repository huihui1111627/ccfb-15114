package sortengine

import (
	"encoding/binary"
	"math"
	"strconv"
)

// Kind is the runtime type of a column value. The encoder dispatches on it; a
// column's expression (or literal) is evaluated once per row before encoding.
type Kind uint8

const (
	KindNull Kind = iota
	KindBool
	KindInt
	KindUint
	KindFloat
	KindString
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindInt:
		return "int"
	case KindUint:
		return "uint"
	case KindFloat:
		return "float"
	case KindString:
		return "string"
	case KindAny:
		return "any"
	}
	return "kind?"
}

// Value is one evaluated sort-key value.
//
//   - Null is never encoded as a value; the column's null placement decides
//     where it goes. Comparison of two nulls uses tag only, so ties are
//     preserved by input order.
//   - Float stores int64 bits. Negative zero is normalised to +0 in
//     NormalizeFloat so -0.0 and +0.0 encode identically.
//   - NaN is legal: it sorts above +Inf (which matches IEEE-754 totalOrder's
//     treatment of quiet NaNs with a positive sign bit) and is tagged apart
//     from ordinary floats so its payload bits never fight the order of
//     numbers.
//   - String carries raw UTF-8 bytes.
type Value struct {
	Kind Kind
	Bool bool
	Int  int64
	Uint uint64
	// Float holds the IEEE-754 bits of a float64.
	Float uint64
	Str   string
}

var Null = Value{Kind: KindNull}

func Bool(b bool) Value   { return Value{Kind: KindBool, Bool: b} }
func Int(i int64) Value   { return Value{Kind: KindInt, Int: i} }
func Uint(u uint64) Value { return Value{Kind: KindUint, Uint: u} }
func Float(f float64) Value {
	return Value{Kind: KindFloat, Float: math.Float64bits(NormalizeFloat(f))}
}
func Str(s string) Value { return Value{Kind: KindString, Str: s} }

// canonicalNaN is the one NaN bit pattern the engine uses. Folding every NaN
// payload (and sign) into it makes NaN compare equal to NaN and greater than
// every number, so that the encoded order and CompareValues agree.
const canonicalNaN = 0x7FF8000000000000

// NormalizeFloat canonicalises -0 to +0 and every NaN to canonicalNaN, so that
// values which compare equal also encode identically.
func NormalizeFloat(f float64) float64 {
	if f == 0 {
		return 0 // clears the sign bit for -0.0
	}
	if f != f {
		return math.Float64frombits(canonicalNaN)
	}
	return f
}

// IsNull reports whether v is SQL NULL (or a missing value produced by an
// expression over NULL inputs).
func (v Value) IsNull() bool { return v.Kind == KindNull }

func (v Value) String() string {
	switch v.Kind {
	case KindNull:
		return "NULL"
	case KindBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindUint:
		return strconv.FormatUint(v.Uint, 10)
	case KindFloat:
		f := math.Float64frombits(v.Float)
		if math.IsNaN(f) {
			return "NaN"
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	case KindString:
		return v.Str
	}
	return "?"
}

// AppendRaw appends the value's bytes without a type tag. It is only used for
// columns restricted to one kind (the common case in an execution engine,
// where a column has a schema type), so that the encoder can skip the tag.
func (v Value) AppendRaw(dst []byte) []byte {
	switch v.Kind {
	case KindBool:
		if v.Bool {
			return append(dst, 1)
		}
		return append(dst, 0)
	case KindInt:
		return binary.BigEndian.AppendUint64(dst, uint64(v.Int)^signBit)
	case KindUint:
		return binary.BigEndian.AppendUint64(dst, v.Uint)
	case KindFloat:
		return binary.BigEndian.AppendUint64(dst, orderFloat(v.Float))
	case KindString:
		return append(dst, v.Str...)
	}
	return dst
}

const signBit = uint64(1) << 63

// orderFloat maps IEEE-754 bits to an unsigned integer with the same order as
// the numeric total order used by the encoder:
//
//	NaN (quiet, positive)  >  +Inf  >  finite  >  -Inf
//
// All negative numbers (sign bit set) are bitwise inverted; non-negative
// numbers are flipped by setting the sign bit. Negative zero has already been
// folded into +0 by NormalizeFloat, so the two zeros are equal.
func orderFloat(bits uint64) uint64 {
	if bits&signBit != 0 {
		return ^bits
	}
	return bits | signBit
}

// Coerce converts v to the column's declared kind. A column in an execution
// engine has a schema type; expressions can still produce a wider type (for
// example 1/2 produces a float in an int column), so the encoder applies the
// same conversion a cast would. A conversion that has no defined value
// (a string in a numeric column, a negative number in a uint column, a NaN or
// out-of-range float in an integer column) becomes NULL and takes the
// column's NULL placement.
func Coerce(v Value, k Kind) Value {
	if v.Kind == KindNull || v.Kind == k || k == KindAny {
		return v
	}
	switch k {
	case KindBool:
		return Null
	case KindInt:
		switch v.Kind {
		case KindUint:
			if v.Uint <= math.MaxInt64 {
				return Int(int64(v.Uint))
			}
			return Null
		case KindFloat:
			f := NormalizeFloat(math.Float64frombits(v.Float))
			if f != f || f < -9223372036854775808.0 || f >= 9223372036854775808.0 {
				return Null
			}
			return Int(int64(f))
		}
		return Null
	case KindUint:
		switch v.Kind {
		case KindInt:
			if v.Int >= 0 {
				return Uint(uint64(v.Int))
			}
			return Null
		case KindFloat:
			f := NormalizeFloat(math.Float64frombits(v.Float))
			if f != f || f < 0 || f >= 18446744073709551616.0 {
				return Null
			}
			return Uint(uint64(f))
		}
		return Null
	case KindFloat:
		switch v.Kind {
		case KindInt:
			return Float(float64(v.Int))
		case KindUint:
			return Float(float64(v.Uint))
		}
		return Null
	case KindString:
		return Null
	}
	return Null
}
