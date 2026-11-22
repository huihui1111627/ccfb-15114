package sortengine

import (
	"math"
	"math/rand"
	"testing"
)

// TestAnyColumnAgainstReference sweeps mixed-kind columns. A homogeneous Any
// column reverses correctly under DESC; a column that mixes value families is
// only meaningful ascending (DESC inverts the tag bytes too, which is why the
// demo warns that Any is for heterogeneous ascending keys).
func TestAnyColumnAgainstReference(t *testing.T) {
	schema := NewSchema("v")
	rng := rand.New(rand.NewSource(21))
	for _, dir := range []Direction{Asc} {
		col := Column{Name: "v", Kind: KindAny, Dir: dir}
		s := mustNew(t, Spec{Columns: []Column{col}}, schema)
		rows := make([]Row, 400)
		for i := range rows {
			switch rng.Intn(6) {
			case 0:
				rows[i] = Row{Null}
			case 1:
				rows[i] = Row{Bool(rng.Intn(2) == 0)}
			case 2:
				rows[i] = Row{Int(int64(rng.Intn(9) - 4))}
			case 3:
				rows[i] = Row{Uint(uint64(rng.Intn(9)))}
			case 4:
				rows[i] = Row{Float(float64(rng.Intn(9)) / 2)}
			default:
				rows[i] = Row{Str([]string{"", "a", "ab", "b"}[rng.Intn(4)])}
			}
		}
		checkOrder(t, s, rows, Options{})
	}
}

// TestParallelLeavesCoverEveryRowAndOrder checks the parallel partition
// invariant directly: the leaves must partition the row set, and the first
// byte of every leaf must be non-decreasing.
func TestParallelLeavesCoverEveryRowAndOrder(t *testing.T) {
	schema := NewSchema("z", "v")
	s := mustNew(t, Spec{Columns: []Column{
		{Name: "z", Kind: KindInt},
		{Name: "v", Kind: KindString},
	}, Parallel: true, MinParallelRows: 1}, schema)
	rng := rand.New(rand.NewSource(5))
	rows := make([]Row, 40000)
	for i := range rows {
		rows[i] = Row{
			Int(int64(rng.Intn(300))),
			Str([]string{"", "a", "aa", "ab"}[rng.Intn(4)]),
		}
	}
	res := s.Sort(rows, Options{Workers: 4})
	if !res.Stats.Parallel || res.Stats.Buckets < 2 {
		t.Fatalf("expected a parallel multi-bucket run, got %+v", res.Stats)
	}
	seen := make([]bool, len(rows))
	prev := byte(0)
	for i, idx := range res.Order {
		if seen[idx] {
			t.Fatalf("row %d appears twice", idx)
		}
		seen[idx] = true
		res2 := s.Sort(rows[idx:idx+1], Options{NoSort: true, Samples: 1})
		b := byte(0)
		if v := res2.Samples[0].Key; len(v) >= 2 {
			b = hexByte(v[:2])
		}
		if i > 0 && b < prev {
			t.Fatalf("first-byte order violated at %d: %02x after %02x", i, b, prev)
		}
		prev = b
	}
}

func hexByte(s string) byte {
	var b byte
	for i := 0; i < len(s); i++ {
		b <<= 4
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			b |= c - '0'
		case c >= 'a' && c <= 'f':
			b |= c - 'a' + 10
		}
	}
	return b
}

func TestUintAndIntRounding(t *testing.T) {
	schema := NewSchema("u", "i")
	s := mustNew(t, Spec{Columns: []Column{
		{Name: "u", Kind: KindUint, Dir: Desc},
		{Name: "i", Kind: KindInt},
	}}, schema)
	rows := []Row{
		{Uint(math.MaxUint64), Int(math.MinInt64)},
		{Uint(0), Int(0)},
		{Null, Int(math.MaxInt64)},
		{Uint(1 << 63), Int(-1)},
	}
	checkOrder(t, s, rows, Options{})
}

func TestNullPlacementIsIndependentOfDirection(t *testing.T) {
	schema := NewSchema("v")
	for _, dir := range []Direction{Asc, Desc} {
		for _, nulls := range []NullsOrder{NullsFirst, NullsLast} {
			col := Column{Name: "v", Kind: KindInt, Dir: dir, Nulls: nulls}
			s := mustNew(t, Spec{Columns: []Column{col}}, schema)
			rows := []Row{{Null}, {Int(2)}, {Int(1)}, {Null}, {Int(3)}}
			res := s.Sort(rows, Options{})
			var firstVal, lastVal bool
			firstVal = !rows[res.Order[0]][0].IsNull()
			lastVal = !rows[res.Order[len(res.Order)-1]][0].IsNull()
			if nulls == NullsFirst && (firstVal || !lastVal) {
				t.Fatalf("dir=%v nulls=first: nulls not first (%v)", dir, res.Order)
			}
			if nulls == NullsLast && (!firstVal || lastVal) {
				t.Fatalf("dir=%v nulls=last: nulls not last (%v)", dir, res.Order)
			}
			if dir == Desc && firstVal {
				if !(rows[res.Order[0]][0].Int > rows[res.Order[1]][0].Int) {
					t.Fatalf("desc not descending")
				}
			}
		}
	}
}
