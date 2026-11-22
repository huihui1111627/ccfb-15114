package sortengine

import (
	"bytes"
	"math"
	"sort"
	"testing"
)

func encodeOne(t *testing.T, col Column, schema *Schema, row Row) []byte {
	t.Helper()
	s, err := New(Spec{Columns: []Column{col}}, schema)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var buf encBuf
	return append([]byte(nil), s.appendKeyWithOffsets(&buf, row)...)
}

func TestEscapedStringPrefixOrder(t *testing.T) {
	schema := NewSchema("s")
	col := Column{Name: "s", Kind: KindString}
	pairs := [][2]string{
		{"", "a"},
		{"a", "aa"},
		{"a", "a\x00"},
		{"a\x00", "aa"},
		{"a\x00b", "a\x00"},
		{"a\x00", "a\x01"},
		{"\x00", "\x00\x00"},
		{"ab", "abc"},
		{"b", "aa"},
		{"\xff", "a"},
	}
	for _, p := range pairs {
		ka := encodeOne(t, col, schema, Row{Str(p[0])})
		kb := encodeOne(t, col, schema, Row{Str(p[1])})
		want := bytes.Compare([]byte(p[0]), []byte(p[1]))
		got := bytes.Compare(ka, kb)
		if sign(got) != sign(want) {
			t.Errorf("%q vs %q: encoded cmp=%d want %d (keys %x %x)", p[0], p[1], got, want, ka, kb)
		}
	}
}

func sign(x int) int {
	if x < 0 {
		return -1
	}
	if x > 0 {
		return 1
	}
	return 0
}

func TestNoKeyIsPrefixOfAnother(t *testing.T) {
	// Every column is self-delimiting, so a full multi-column key can never be
	// a prefix of another. This is what lets radix leaves tie-break by input
	// order without re-checking the whole key.
	schema := NewSchema("i", "s", "f")
	spec := Spec{Columns: []Column{
		{Name: "i", Kind: KindInt},
		{Name: "s", Kind: KindString},
		{Name: "f", Kind: KindFloat, Dir: Desc},
	}}
	s, err := New(spec, schema)
	if err != nil {
		t.Fatal(err)
	}
	rows := []Row{
		{Int(1), Str(""), Null},
		{Int(1), Str("a"), Null},
		{Int(-5), Str("aaaaaaaa"), Float(math.Inf(1))},
	}
	var buf encBuf
	keys := make([][]byte, len(rows))
	for i, r := range rows {
		keys[i] = append([]byte(nil), s.appendKeyWithOffsets(&buf, r)...)
	}
	for i := range keys {
		for j := range keys {
			if i == j {
				continue
			}
			if bytes.HasPrefix(keys[i], keys[j]) {
				t.Errorf("key %d (%x) is a prefix of key %d (%x)", j, keys[j], i, keys[i])
			}
		}
	}
}

func TestFloatOrderingEdgeCases(t *testing.T) {
	schema := NewSchema("f")
	col := Column{Name: "f", Kind: KindFloat}
	vals := []float64{
		math.Inf(-1), -math.MaxFloat64, -1e308, -1.5, -1, -math.SmallestNonzeroFloat64,
		0, math.Copysign(0, -1), math.SmallestNonzeroFloat64, 1, 1.5, math.MaxFloat64,
		math.Inf(1), math.NaN(), math.Float64frombits(0x7FF8000000000001),
		math.Float64frombits(0xFFF8000000000000),
	}
	// Deduplicate: all NaNs and both zeros must encode identically.
	var uniq []float64
	for _, v := range vals {
		dup := false
		for _, u := range uniq {
			if sameOrdered(u, v) {
				dup = true
				break
			}
		}
		if !dup {
			uniq = append(uniq, v)
		}
	}
	// Index-keyed: a map keyed by NaN would not compare equal to itself.
	keys := make([][]byte, len(uniq))
	for i, v := range uniq {
		keys[i] = encodeOne(t, col, schema, Row{Float(v)})
	}
	// Ascending
	for i := 0; i < len(uniq); i++ {
		for j := 0; j < len(uniq); j++ {
			a, b := uniq[i], uniq[j]
			c := bytes.Compare(keys[i], keys[j])
			want := cmpFloat(NormalizeFloat(a), NormalizeFloat(b))
			if sign(c) != want {
				t.Errorf("asc %v vs %v: got %d want %d", a, b, c, want)
			}
		}
	}
	// Descending
	colDesc := col
	colDesc.Dir = Desc
	for i := 0; i < len(uniq); i++ {
		for j := 0; j < len(uniq); j++ {
			a, b := uniq[i], uniq[j]
			ka := encodeOne(t, colDesc, schema, Row{Float(a)})
			kb := encodeOne(t, colDesc, schema, Row{Float(b)})
			c := bytes.Compare(ka, kb)
			want := cmpFloat(NormalizeFloat(a), NormalizeFloat(b))
			if sign(c) != -want {
				t.Errorf("desc %v vs %v: got %d want %d", a, b, c, -want)
			}
		}
	}
}

func sameOrdered(a, b float64) bool {
	return cmpFloat(NormalizeFloat(a), NormalizeFloat(b)) == 0
}

func TestInternalNumericOrderMatchesIntFloat(t *testing.T) {
	// Guard the exact cross-type comparators used by the reference oracle.
	type pair struct {
		i    int64
		f    float64
		want int
	}
	cases := []pair{
		{1<<53 + 1, float64(1 << 53), 1},
		{1<<53 + 1, float64(1<<53) + 2, -1},
		{math.MinInt64, math.Nextafter(float64(math.MinInt64), math.Inf(-1)), 1},
		{math.MaxInt64, 9.223372036854775808e18, -1},
		{0, -0.5, 1},
		{-1, -0.5, -1},
		{5, 5.0, 0},
		{math.MinInt64, math.Inf(-1), 1},
		{math.MaxInt64, math.NaN(), -1},
	}
	for _, c := range cases {
		if got := cmpIntFloat(c.i, c.f); got != c.want {
			t.Errorf("cmpIntFloat(%d, %v) = %d want %d", c.i, c.f, got, c.want)
		}
	}
	if got := cmpUintFloat(math.MaxUint64, 1.8446744073709552e19); got != -1 {
		t.Errorf("cmpUintFloat(maxuint64, 2^64) = %d want -1", got)
	}
}

func TestPinyinColumnOrderBinary(t *testing.T) {
	schema := NewSchema("n")
	col := Column{Name: "n", Kind: KindString, Pinyin: true}
	names := []string{"张伟", "李娜", "王芳", "刘洋", "陈静", "杨明", "赵磊", "黄敏", "周杰", "吴昊", "阿", "阿爸", "安"}
	keys := make([][]byte, len(names))
	for i, n := range names {
		keys[i] = encodeOne(t, col, schema, Row{Str(n)})
	}
	for i := range names {
		for j := range names {
			got := bytes.Compare(keys[i], keys[j])
			want := comparePinyin(names[i], names[j])
			if sign(got) != sign(want) {
				t.Errorf("pinyin %q vs %q: bytes %d ref %d", names[i], names[j], got, want)
			}
		}
	}
}

func TestPinyinKnownOrder(t *testing.T) {
	// Dictionary order by toneless syllable then tone (Ignoring case, the
	// syllables are a, bai, chen, hai, huang, li, liu, wang, wu, zhang, zhao, zhou).
	names := []string{"阿", "白", "陈", "海", "黄", "李", "刘", "王", "吴", "张", "赵", "周"}
	copyNames := append([]string(nil), names...)
	sort.Slice(copyNames, func(i, j int) bool { return comparePinyin(copyNames[i], copyNames[j]) < 0 })
	got := "阿白陈海黄李刘王吴张赵周"
	var joined string
	for _, n := range copyNames {
		joined += n
	}
	if joined != got {
		t.Errorf("pinyin order = %s want %s", joined, got)
	}
}

func TestPinyinToneBreaksTie(t *testing.T) {
	// 妈 mā(1) < 麻 má(2) < 马 mǎ(3) < 骂 mà(4)
	got := comparePinyin("妈", "麻")
	if got >= 0 {
		t.Errorf("妈 should sort before 麻, got %d", got)
	}
	if comparePinyin("麻", "马") >= 0 || comparePinyin("马", "骂") >= 0 {
		t.Errorf("tone order wrong: %d %d", comparePinyin("麻", "马"), comparePinyin("马", "骂"))
	}
}

func TestPinyinTableCoverage(t *testing.T) {
	for _, r := range "鸽舍名字赛区归巢分速长度用时机钟" {
		if _, ok := pinyinLookup(r); !ok {
			t.Errorf("rune %q (U+%04X) has no pinyin reading in the table", r, r)
		}
	}
	// A CJK ext-B char should still resolve through the extended table.
	if _, ok := pinyinLookup(0x20000); !ok {
		t.Logf("U+20000 has no reading (acceptable; falls back to code point)")
	}
}
