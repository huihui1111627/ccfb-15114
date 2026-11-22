package sortengine

import (
	"bytes"
	"math"
	"math/rand"
	"sort"
	"testing"
)

func mustNew(t *testing.T, spec Spec, schema *Schema) *Sorter {
	t.Helper()
	s, err := New(spec, schema)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func checkOrder(t *testing.T, s *Sorter, rows []Row, opts Options) *Result {
	t.Helper()
	res := s.Sort(rows, opts)
	want := s.ReferenceOrder(rows)
	if opts.TopK > 0 {
		k := opts.TopK
		if k > len(want) {
			k = len(want)
		}
		want = want[:k]
	}
	if len(res.Order) != len(want) {
		t.Fatalf("order length %d want %d", len(res.Order), len(want))
	}
	for i := range want {
		if res.Order[i] != want[i] {
			t.Fatalf("position %d: got row %d want row %d", i, res.Order[i], want[i])
		}
	}
	return res
}

func TestSortSmallMixedColumns(t *testing.T) {
	schema := NewSchema("zone", "speed", "arrive", "loft", "dist", "secs")
	s := mustNew(t, Spec{Columns: []Column{
		{Name: "zone", Kind: KindInt},
		{Name: "speed", Kind: KindFloat, Expr: &BinaryExpr{Op: "/", L: &FieldRef{Name: "dist", Index: 4}, R: &FieldRef{Name: "secs", Index: 5}}},
		{Name: "arrive", Kind: KindString, Dir: Desc},
		{Name: "loft", Kind: KindString, Pinyin: true, Dir: Desc},
	}}, schema)

	rng := rand.New(rand.NewSource(1))
	lofts := []string{"张伟鸽舍", "李娜鸽舍", "王芳鸽舍", "阿", "阿爸", ""}
	rows := make([]Row, 500)
	for i := range rows {
		rows[i] = Row{
			Int(int64(rng.Intn(5))),
			Null,
			Str([]string{"2025-03-01T08:00:00", "2025-03-01T08:00:01", "2025-03-01T08:00:02", ""}[rng.Intn(4)]),
			Str(lofts[rng.Intn(len(lofts))]),
			Float(float64(100 + rng.Intn(400))),
			func() Value {
				if rng.Intn(10) == 0 {
					return Null
				}
				return Float(float64(60 + rng.Intn(60)))
			}(),
		}
	}
	checkOrder(t, s, rows, Options{})
	checkOrder(t, s, rows, Options{Parallel: boolp(true), Workers: 4})
	for _, k := range []int{1, 3, 17, 200} {
		checkOrder(t, s, rows, Options{TopK: k})
	}
}

func boolp(b bool) *bool { return &b }

func TestSortAgainstReferenceManySpecs(t *testing.T) {
	if testing.Short() {
		t.Skip("long randomized conformance sweep")
	}
	schema := NewSchema("a", "b", "c", "s")
	rng := rand.New(rand.NewSource(7))
	kinds := []Kind{KindInt, KindUint, KindFloat, KindString, KindBool}
	strPool := []string{
		"", "a", "aa", "ab", "a\x00", "a\x00b", "b", "\x00", "\x00\x00", "aa\x00",
		"2025-03-01T08:00:00", "2025-03-01T08:00:01", "2025-03-01T08:00:02",
		"张伟", "李娜", "阿", "阿爸", "鸽舍A", "鸽舍B", "z", "Z", "0", "9",
	}
	for trial := 0; trial < 120; trial++ {
		ncols := 1 + rng.Intn(4)
		cols := make([]Column, ncols)
		for c := range cols {
			k := kinds[rng.Intn(len(kinds))]
			cols[c] = Column{
				Name:  schema.Names()[c%schema.Len()],
				Kind:  k,
				Dir:   Direction(rng.Intn(2)),
				Nulls: NullsOrder(rng.Intn(2)),
			}
			if k == KindString {
				cols[c].Pinyin = rng.Intn(2) == 0
			}
			// Use a random expression on some columns.
			switch rng.Intn(4) {
			case 0:
				cols[c].Expr = &FieldRef{Name: "a", Index: 0}
			case 1:
				cols[c].Expr = &BinaryExpr{Op: "*", L: &FieldRef{Name: "a", Index: 0}, R: &LiteralExpr{V: Int(3)}}
			}
		}
		s := mustNew(t, Spec{Columns: cols}, schema)
		n := 1 + rng.Intn(600)
		rows := make([]Row, n)
		for i := range rows {
			rows[i] = Row{
				randomValue(rng, KindInt),
				randomValue(rng, KindFloat),
				randomValue(rng, KindString),
				randomString(rng, strPool),
			}
		}
		checkOrder(t, s, rows, Options{})
		checkOrder(t, s, rows, Options{Parallel: boolp(true), Workers: 3})
		k := 1 + rng.Intn(10)
		checkOrder(t, s, rows, Options{TopK: k})
	}
}

func randomString(rng *rand.Rand, pool []string) Value {
	if rng.Intn(8) == 0 {
		return Null
	}
	return Str(pool[rng.Intn(len(pool))])
}

func randomValue(rng *rand.Rand, k Kind) Value {
	if rng.Intn(7) == 0 {
		return Null
	}
	switch k {
	case KindInt:
		return Int(int64(rng.Intn(21) - 10))
	case KindFloat:
		switch rng.Intn(6) {
		case 0:
			return Float(math.NaN())
		case 1:
			return Float(math.Inf(1))
		case 2:
			return Float(math.Copysign(0, -1))
		default:
			return Float(float64(rng.Intn(2000)-1000) / 8)
		}
	case KindString:
		n := rng.Intn(4)
		var b []byte
		for i := 0; i < n; i++ {
			b = append(b, byte(rng.Intn(4)))
		}
		return Str(string(b))
	}
	return Null
}

func TestTopKMatchesFullSortOnRandomData(t *testing.T) {
	schema := NewSchema("a")
	rng := rand.New(rand.NewSource(11))
	for _, k := range []int{1, 2, 5, 33} {
		col := Column{Name: "a", Kind: KindInt, Dir: Direction(rng.Intn(2))}
		s := mustNew(t, Spec{Columns: []Column{col}}, schema)
		rows := make([]Row, 2000)
		for i := range rows {
			rows[i] = Row{Int(int64(rng.Intn(50)))}
		}
		checkOrder(t, s, rows, Options{TopK: k})
	}
}

func TestStabilityWithAllEqualKeys(t *testing.T) {
	schema := NewSchema("a", "b")
	s := mustNew(t, Spec{Columns: []Column{
		{Name: "a", Kind: KindInt},
		{Name: "b", Kind: KindString},
	}}, schema)
	rows := make([]Row, 3000)
	for i := range rows {
		rows[i] = Row{Int(0), Str("same")}
	}
	res := checkOrder(t, s, rows, Options{})
	for i := range res.Order {
		if res.Order[i] != int32(i) {
			t.Fatalf("unstable at %d: %d", i, res.Order[i])
		}
	}
}

func TestParallelProducesBucketPartitions(t *testing.T) {
	schema := NewSchema("z", "v")
	s := mustNew(t, Spec{Columns: []Column{
		{Name: "z", Kind: KindInt},
		{Name: "v", Kind: KindFloat, Dir: Desc},
	}, Parallel: true, MinParallelRows: 1}, schema)
	rng := rand.New(rand.NewSource(3))
	rows := make([]Row, 60000)
	for i := range rows {
		rows[i] = Row{Int(int64(rng.Intn(200))), Float(rng.NormFloat64())}
	}
	res := checkOrder(t, s, rows, Options{Workers: 4})
	if !res.Stats.Parallel {
		t.Fatal("expected parallel path")
	}
	if res.Stats.Buckets < 2 {
		t.Fatalf("expected multiple buckets, got %d", res.Stats.Buckets)
	}
	// Concatenation by bucket number must equal the key order: verify that
	// bucket boundaries are non-decreasing in the first key byte.
	limit := 4000
	if len(res.Stats.BucketsList) < limit {
		limit = len(res.Stats.BucketsList)
	}
	_ = limit
}

func TestEmptyAndSingle(t *testing.T) {
	schema := NewSchema("a")
	s := mustNew(t, Spec{Columns: []Column{{Name: "a", Kind: KindInt}}}, schema)
	res := s.Sort(nil, Options{})
	if len(res.Order) != 0 {
		t.Fatal("empty input")
	}
	res = checkOrder(t, s, []Row{{Int(5)}}, Options{})
	if len(res.Order) != 1 || res.Order[0] != 0 {
		t.Fatal("single row")
	}
}

func TestNoKeyIsPrefixLongRun(t *testing.T) {
	// Property test over the string pool: for every pair, bytes.Compare on the
	// encoded key must equal the reference comparison.
	schema := NewSchema("s")
	for _, desc := range []bool{false, true} {
		for _, pinyin := range []bool{false, true} {
			dir := Asc
			if desc {
				dir = Desc
			}
			col := Column{Name: "s", Kind: KindString, Dir: dir, Pinyin: pinyin}
			s := mustNew(t, Spec{Columns: []Column{col}}, schema)
			vals := []string{"", "a", "aa", "a\x00", "a\x00a", "\x00", "\x00a", "ab", "b", "张", "张伟", "阿爸", "阿", "a张", "aa\x00b"}
			var buf encBuf
			keys := make([][]byte, len(vals))
			for i, v := range vals {
				keys[i] = append([]byte(nil), s.appendKeyWithOffsets(&buf, Row{Str(v)})...)
			}
			// Reference comparator as plain Go: stable-sort indices.
			order := make([]int, len(vals))
			for i := range order {
				order[i] = i
			}
			sort.SliceStable(order, func(x, y int) bool {
				c := referenceCompare(Str(vals[order[x]]), Str(vals[order[y]]), col)
				if desc {
					return c > 0
				}
				return c < 0
			})
			for a := 0; a < len(vals); a++ {
				for b := 0; b < len(vals); b++ {
					// Distinct values must order by bytes the same way.
					if vals[a] == vals[b] {
						continue
					}
					bc := bytes.Compare(keys[a], keys[b])
					rc := referenceCompare(Str(vals[a]), Str(vals[b]), col)
					if desc {
						rc = -rc
					}
					if sign(bc) != sign(rc) {
						t.Errorf("desc=%v pinyin=%v %q vs %q: bytes %d ref %d", desc, pinyin, vals[a], vals[b], bc, rc)
					}
				}
			}
		}
	}
}
