// Package harness generates random sort specifications and row data for the
// verification program and the demo server. The generator deliberately mixes
// NULLs, negative zero, NaN, strings with prefix relationships, embedded NUL
// bytes and heavy key duplication, so that every encoding rule is exercised.
package harness

import (
	"fmt"
	"math"
	"math/rand"
	"strings"

	"pigeonsort/sortengine"
)

// FieldCatalog describes the demo table: one race season of homing records.
type Field struct {
	Name string
	Kind sortengine.Kind
	Desc string
}

// Fields is the input schema used by the demo and by the generators.
var Fields = []Field{
	{"zone", sortengine.KindInt, "赛区编号"},
	{"distance", sortengine.KindFloat, "司放空距(公里)"},
	{"duration", sortengine.KindFloat, "飞行用时(分钟)"},
	{"arrive", sortengine.KindString, "归巢时刻(ISO8601)"},
	{"loft", sortengine.KindString, "鸽舍名(汉字)"},
	{"member", sortengine.KindString, "会员编号"},
	{"ring", sortengine.KindUint, "足环号"},
	{"eligible", sortengine.KindBool, "是否有格"},
	{"net", sortengine.KindFloat, "净分速"},
	{"note", sortengine.KindString, "备注"},
}

// Schema returns the input schema for Fields.
func Schema() *sortengine.Schema {
	names := make([]string, len(Fields))
	for i, f := range Fields {
		names[i] = f.Name
	}
	return sortengine.NewSchema(names...)
}

// ExprCatalog is a pool of sort expressions over Fields. Entries with NULL
// operands evaluate to NULL.
var ExprCatalog = []struct {
	Name string
	Src  string
	Kind sortengine.Kind
}{
	{"赛区", "zone", sortengine.KindInt},
	{"分速=距离/用时", "distance / duration", sortengine.KindFloat},
	{"分速(米/分)", "distance * 1000 / duration", sortengine.KindFloat},
	{"放大分速", "(distance - 500) * 1000 / duration", sortengine.KindFloat},
	{"归巢时刻", "arrive", sortengine.KindString},
	{"鸽舍名", "loft", sortengine.KindString},
	{"鸽舍名(空值替换)", "coalesce(loft, '未知鸽舍')", sortengine.KindString},
	{"会员编号", "member", sortengine.KindString},
	{"足环号", "ring", sortengine.KindUint},
	{"是否有格", "eligible", sortengine.KindBool},
	{"净分速", "net", sortengine.KindFloat},
	{"距离", "distance", sortengine.KindFloat},
	{"用时", "duration", sortengine.KindFloat},
	{"用时补零", "coalesce(duration, 0)", sortengine.KindFloat},
	{"分速取整", "round(distance / duration, 2)", sortengine.KindFloat},
	{"赛区+1", "zone + 1", sortengine.KindInt},
	{"距离绝对值", "abs(distance - 500)", sortengine.KindFloat},
	{"满分速标记", "distance / duration >= 1000", sortengine.KindBool},
	{"备注", "note", sortengine.KindString},
	{"空值探测", "nullif(zone, 0)", sortengine.KindInt},
	{"混合类型", "zone", sortengine.KindAny},
}

// LoftNames are Han-character loft names covering many syllables, tones and
// prefix relationships (阿 is a prefix of 阿爸).
var LoftNames = []string{
	"阿", "阿爸", "阿爸鸽舍", "白鸽苑", "陈氏鸽舍", "长空鸽业", "东方红", "东方红鸽舍",
	"飞龙", "飞龙在天", "高飞", "海翔", "海翔鸽舍", "黄家", "金翼", "李记", "刘氏",
	"龙腾", "马到成功", "平和", "千里", "日出", "赛鸽之家", "天骄", "王", "王鸽舍",
	"吴氏", "翔云", "杨帆", "阳光", "张伟", "张伟鸽舍", "赵家", "周记", "追风",
}

// PrefixPool is a string pool full of prefix relationships and embedded NULs.
var PrefixPool = []string{
	"", "A", "AA", "AAA", "A\x00", "A\x00A", "\x00", "\x00\x00", "AB", "ABC",
	"2025-03-01T08:00:00", "2025-03-01T08:00:00.5", "2025-03-01T08:00:01",
	"2025-03-01T08:00:01Z", "a", "aa", "a\x00b", "b",
}

// Case is one generated conformance case.
type Case struct {
	Index  int
	Title  string
	Spec   sortengine.Spec
	Rows   []sortengine.Row
	TopK   int
	Schema *sortengine.Schema
}

// GenerateCase builds a random sort spec over a random subset of the
// expression catalog, plus a random dataset of n rows.
func GenerateCase(rng *rand.Rand, index, n int) Case {
	schema := Schema()
	ncols := 1 + rng.Intn(5)
	cols := make([]sortengine.Column, 0, ncols)
	used := map[int]bool{}
	for len(cols) < ncols {
		pi := rng.Intn(len(ExprCatalog))
		if used[pi] {
			continue
		}
		used[pi] = true
		cat := ExprCatalog[pi]
		col, err := sortengine.ParseColumn(
			cat.Name, cat.Kind.String(), cat.Src,
			sortengine.Direction(rng.Intn(2)),
			sortengine.NullsOrder(rng.Intn(2)),
			cat.Kind == sortengine.KindString && rng.Intn(2) == 0,
		)
		if err != nil {
			panic(err)
		}
		// The title carries the Chinese expression; keep the internal name
		// ASCII for stable JSON keys.
		col.Name = fmt.Sprintf("c%d_%s", len(cols), cat.Name)
		cols = append(cols, col)
	}
	spec := sortengine.Spec{Columns: cols}
	if rng.Intn(2) == 0 {
		spec.Parallel = true
		spec.MinParallelRows = 1 + rng.Intn(4000)
	}
	var title strings.Builder
	for i, c := range cols {
		if i > 0 {
			title.WriteString(", ")
		}
		fmt.Fprintf(&title, "%s %s %s", c.Expr.String(), c.Dir, c.Nulls)
		if c.Pinyin {
			title.WriteString(" 拼音")
		}
	}
	return Case{
		Index:  index,
		Title:  title.String(),
		Spec:   spec,
		Rows:   GenerateRows(rng, n),
		TopK:   pickTopK(rng, n),
		Schema: schema,
	}
}

func pickTopK(rng *rand.Rand, n int) int {
	if rng.Intn(3) != 0 {
		return 0
	}
	k := 1 + rng.Intn(20)
	if k > n {
		k = n
	}
	return k
}

// GenerateRows builds n rows with values drawn to stress the encoder.
func GenerateRows(rng *rand.Rand, n int) []sortengine.Row {
	rows := make([]sortengine.Row, n)
	for i := range rows {
		rows[i] = GenerateRow(rng)
	}
	return rows
}

// GenerateRow builds one row.
func GenerateRow(rng *rand.Rand) sortengine.Row {
	row := make(sortengine.Row, len(Fields))
	for i, f := range Fields {
		row[i] = randomField(rng, f.Name, f.Kind)
	}
	return row
}

func randomField(rng *rand.Rand, name string, k sortengine.Kind) sortengine.Value {
	// Roughly one in eight values is NULL in every column.
	if rng.Intn(8) == 0 {
		return sortengine.Null
	}
	switch name {
	case "zone":
		// Few distinct values: heavy duplication in the leading key.
		return sortengine.Int(int64(rng.Intn(8)))
	case "distance":
		switch rng.Intn(40) {
		case 0:
			return sortengine.Float(math.Copysign(0, -1)) // negative zero
		case 1:
			return sortengine.Float(math.NaN()) // not a number
		case 2:
			return sortengine.Float(math.Inf(1))
		}
		return sortengine.Float(300 + math.Round(rng.Float64()*400*100)/100)
	case "duration":
		switch rng.Intn(30) {
		case 0:
			return sortengine.Float(0) // forces distance/duration to NULL
		case 1:
			return sortengine.Float(math.Copysign(0, -1))
		}
		return sortengine.Float(30 + math.Round(rng.Float64()*330*10)/10)
	case "arrive":
		sec := rng.Intn(5)
		// Prefix family: same second with and without a fractional part.
		return sortengine.Str(fmt.Sprintf("2025-03-01T08:00:%02d%s", sec, []string{"", ".5", ".25", "Z"}[rng.Intn(4)]))
	case "loft":
		// Small pool: duplicated keys, prefix pairs, some pinyin columns.
		return sortengine.Str(LoftNames[rng.Intn(len(LoftNames))])
	case "member":
		return sortengine.Str(fmt.Sprintf("M%05d", rng.Intn(5000)))
	case "ring":
		return sortengine.Uint(uint64(rng.Intn(200000)))
	case "eligible":
		return sortengine.Bool(rng.Intn(2) == 0)
	case "net":
		switch rng.Intn(20) {
		case 0:
			return sortengine.Float(math.NaN())
		case 1:
			return sortengine.Float(math.Copysign(0, -1))
		}
		return sortengine.Float(math.Round(rng.NormFloat64()*100*100) / 100)
	case "note":
		return sortengine.Str(PrefixPool[rng.Intn(len(PrefixPool))])
	}
	return sortengine.Null
}

// BuildSorter compiles a case.
func (c Case) BuildSorter() (*sortengine.Sorter, error) {
	return sortengine.New(c.Spec, c.Schema)
}
