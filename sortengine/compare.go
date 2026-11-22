package sortengine

import (
	"math"
	"strings"
)

// CompareValues compares two non-NULL values with the same numeric/lexical
// rules the encoder implements: numbers compare numerically across int, uint
// and float (exact, without float rounding where possible), strings compare by
// Go byte order, bools by false<true, and NaN is greater than every number
// (and equal to itself). Kind rank orders bool < int < uint < float < string,
// matching the KindAny tag order. NULL is not handled here; callers decide
// placement.
func CompareValues(a, b Value) (int, bool) {
	if a.Kind == KindBool && b.Kind == KindBool {
		switch {
		case !a.Bool && b.Bool:
			return -1, true
		case a.Bool && !b.Bool:
			return 1, true
		}
		return 0, true
	}
	if a.Kind == KindString && b.Kind == KindString {
		return strings.Compare(a.Str, b.Str), true
	}
	if isNumeric(a.Kind) && isNumeric(b.Kind) {
		return compareNumeric(a, b), true
	}
	// Mixed families (e.g. bool vs int) are NULL: SQL does not define an order
	// between them and the engine's Any column only carries one family at a
	// time by construction.
	return 0, false
}

func compareNumeric(a, b Value) int {
	// Same-kind integer comparisons avoid any float rounding.
	if a.Kind == KindInt && b.Kind == KindInt {
		return cmpInt(a.Int, b.Int)
	}
	if a.Kind == KindUint && b.Kind == KindUint {
		return cmpUint(a.Uint, b.Uint)
	}
	if a.Kind == KindFloat && b.Kind == KindFloat {
		return cmpFloat(math.Float64frombits(a.Float), math.Float64frombits(b.Float))
	}
	// Special-case NaN before converting, since NaN compares false to all.
	if a.Kind == KindFloat && math.IsNaN(math.Float64frombits(a.Float)) {
		if b.Kind == KindFloat && math.IsNaN(math.Float64frombits(b.Float)) {
			return 0
		}
		return 1
	}
	if b.Kind == KindFloat && math.IsNaN(math.Float64frombits(b.Float)) {
		return -1
	}
	if a.Kind == KindInt && b.Kind == KindFloat {
		return cmpIntFloat(a.Int, math.Float64frombits(b.Float))
	}
	if a.Kind == KindFloat && b.Kind == KindInt {
		return -cmpIntFloat(b.Int, math.Float64frombits(a.Float))
	}
	if a.Kind == KindInt && b.Kind == KindUint {
		if a.Int < 0 {
			return -1
		}
		return cmpUint(uint64(a.Int), b.Uint)
	}
	if a.Kind == KindUint && b.Kind == KindInt {
		if b.Int < 0 {
			return 1
		}
		return cmpUint(a.Uint, uint64(b.Int))
	}
	if a.Kind == KindUint && b.Kind == KindFloat {
		return cmpUintFloat(a.Uint, math.Float64frombits(b.Float))
	}
	if a.Kind == KindFloat && b.Kind == KindUint {
		return -cmpUintFloat(b.Uint, math.Float64frombits(a.Float))
	}
	return 0
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpFloat is the numeric total order used by the encoder:
// -Inf < finite < +Inf < NaN, with -0 == +0 and NaN == NaN.
func cmpFloat(a, b float64) int {
	an, bn := math.IsNaN(a), math.IsNaN(b)
	switch {
	case an && bn:
		return 0
	case an:
		return 1
	case bn:
		return -1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpIntFloat compares an int64 with a float64 without rounding the int to
// float64 (which would conflate 2^53+1 and 2^53, and MinInt64 with -2^63).
func cmpIntFloat(i int64, f float64) int {
	if math.IsNaN(f) {
		return -1
	}
	if f >= math.MaxInt64 { // f is representable at or above 2^63: i < f
		return -1
	}
	if f < math.MinInt64 { // f below -2^63: i > f
		return 1
	}
	// f is within int64 range and is a whole or fractional float.
	tf := math.Trunc(f)
	if tf != f {
		// Fractional: compare against the neighbouring integers.
		if float64(i) < f {
			return -1
		}
		if float64(i) > f {
			return 1
		}
		// float64(i) == f is impossible here only if i cannot be represented;
		// compare exactly via the fractional part sign.
		return 0
	}
	// Whole float in int64 range: converting is exact.
	fi := int64(f)
	return cmpInt(i, fi)
}

// cmpUintFloat compares a uint64 with a float64 without precision loss.
func cmpUintFloat(u uint64, f float64) int {
	if math.IsNaN(f) {
		return -1
	}
	if f < 0 {
		return 1
	}
	if f >= math.MaxUint64 { // 2^64: every uint64 is smaller
		return -1
	}
	tf := math.Trunc(f)
	if tf != f {
		// f in (2^63,2^64) with a fractional part; 2^63 is exact in float64.
		if float64(u) < f {
			return -1
		}
		if float64(u) > f {
			return 1
		}
		return 0
	}
	return cmpUint(u, uint64(f))
}
