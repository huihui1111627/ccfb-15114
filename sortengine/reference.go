package sortengine

import (
	"math"
	"sort"
	"strings"
)

// ReferenceOrder computes the expected order with the standard library's
// stable sort, comparing column by column with ordinary Go comparison. It is
// the oracle the encoded-key engine is checked against.
//
// NULL placement follows each column's NullsOrder; DESC reverses only
// non-NULL values. Pinyin columns compare by reading (syllable, then tone,
// then code point for unreadable characters), exactly as the encoder does.
func ReferenceOrder(rows []Row, cols []compiledColumn) []int32 {
	idx := make([]int32, len(rows))
	for i := range idx {
		idx[i] = int32(i)
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return referenceLess(rows[idx[a]], rows[idx[b]], cols)
	})
	return idx
}

// ReferenceOrderSpec is ReferenceOrder for a public Sorter.
func (s *Sorter) ReferenceOrder(rows []Row) []int32 {
	return ReferenceOrder(rows, s.cols)
}

func referenceLess(ra, rb Row, cols []compiledColumn) bool {
	for _, c := range cols {
		va := Coerce(c.expr.Eval(ra), c.col.Kind)
		vb := Coerce(c.expr.Eval(rb), c.col.Kind)
		an, bn := va.IsNull(), vb.IsNull()
		if an || bn {
			if an && bn {
				continue
			}
			if an {
				return c.col.Nulls == NullsFirst
			}
			return c.col.Nulls == NullsLast
		}
		cmp := referenceCompare(va, vb, c.col)
		if cmp == 0 {
			continue
		}
		if c.col.Dir == Desc {
			return cmp > 0
		}
		return cmp < 0
	}
	return false
}

// referenceCompare compares two non-NULL values. Unlike CompareValues it also
// defines an order across kinds (bool < int < uint < float < string by kind
// rank, with cross-numeric comparison), because an Any column can mix them.
func referenceCompare(a, b Value, col Column) int {
	if a.Kind != b.Kind {
		ra, rb := kindRank(a.Kind), kindRank(b.Kind)
		switch {
		case ra < rb:
			return -1
		case ra > rb:
			return 1
		}
		return 0
	}
	switch a.Kind {
	case KindBool:
		if a.Bool == b.Bool {
			return 0
		}
		if !a.Bool {
			return -1
		}
		return 1
	case KindString:
		if col.Pinyin {
			return comparePinyin(a.Str, b.Str)
		}
		return strings.Compare(a.Str, b.Str)
	case KindInt:
		return cmpInt(a.Int, b.Int)
	case KindUint:
		return cmpUint(a.Uint, b.Uint)
	case KindFloat:
		return cmpFloat(NormalizeFloat(floatOf(a)), NormalizeFloat(floatOf(b)))
	}
	return 0
}

func kindRank(k Kind) int {
	switch k {
	case KindBool:
		return 0
	case KindInt:
		return 1
	case KindUint:
		return 2
	case KindFloat:
		return 3
	case KindString:
		return 4
	}
	return 5
}

func floatOf(v Value) float64 {
	return math.Float64frombits(v.Float)
}

// comparePinyin compares two strings by their reading code sequences, which is
// what the encoded pinyin column orders by: prefix first, then the first
// differing code, then the number of runes.
func comparePinyin(a, b string) int {
	ra := []rune(a)
	rb := []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		ca, cb := PinyinCode(ra[i]), PinyinCode(rb[i])
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(ra) < len(rb):
		return -1
	case len(ra) > len(rb):
		return 1
	}
	return 0
}
