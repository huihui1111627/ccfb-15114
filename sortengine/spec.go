package sortengine

import (
	"errors"
	"fmt"
	"strings"
)

// Direction is the per-column sort direction.
type Direction uint8

const (
	Asc Direction = iota
	Desc
)

func (d Direction) String() string {
	if d == Desc {
		return "DESC"
	}
	return "ASC"
}

// NullsOrder says whether NULLs sort before or after every non-NULL value.
// It is independent of Direction: DESC reverses the values, not the NULL
// placement.
type NullsOrder uint8

const (
	NullsFirst NullsOrder = iota
	NullsLast
)

func (n NullsOrder) String() string {
	if n == NullsLast {
		return "NULLS LAST"
	}
	return "NULLS FIRST"
}

// KindAny lets a column carry values of different kinds. The encoder tags each
// value with its kind so that the resulting bytes still define a total order
// (bool < int < uint < float < string).
const KindAny Kind = 6

// Column is one sort key.
//
// The expression is evaluated once per row, before any sorting starts. A NULL
// in any operand makes the whole expression NULL, and a NULL result is placed
// according to NullsFirst rather than encoded as a value.
type Column struct {
	Name string
	// Kind is the declared result kind (KindBool..KindString, or KindAny).
	// It must be set; KindNull is not a valid column kind.
	Kind  Kind
	Expr  Expr
	Dir   Direction
	Nulls NullsOrder
	// Pinyin orders a string column by Mandarin reading (syllable, then tone)
	// instead of by raw bytes. It is ignored for non-string columns.
	Pinyin bool
}

// Spec is a sort definition plus tuning knobs.
type Spec struct {
	Columns []Column
	// Parallel enables the first-byte partition + per-bucket sort path for
	// large inputs. There is no merge step: buckets are produced in byte order
	// and stay adjacent, so the sorted run is their concatenation.
	Parallel bool
	// MinParallelRows is the row count at which Parallel takes effect.
	// Zero means DefaultMinParallelRows.
	MinParallelRows int
}

const DefaultMinParallelRows = 50000

// Schema names the input row columns (the fields expressions read from).
type Schema struct {
	names []string
	index map[string]int
}

func NewSchema(names ...string) *Schema {
	s := &Schema{names: append([]string(nil), names...), index: make(map[string]int, len(names))}
	for i, n := range names {
		s.index[n] = i
	}
	return s
}

func (s *Schema) Len() int { return len(s.names) }

func (s *Schema) Names() []string { return append([]string(nil), s.names...) }

func (s *Schema) Index(name string) (int, bool) {
	i, ok := s.index[name]
	return i, ok
}

// Row is one input row. Its length must be at least the schema width.
type Row []Value

type compiledColumn struct {
	col  Column
	expr Expr
}

// Sorter is a compiled sort specification, reusable across calls.
type Sorter struct {
	spec Spec
	cols []compiledColumn
}

// New compiles a spec against a schema, resolving field references inside
// expressions.
func New(spec Spec, schema *Schema) (*Sorter, error) {
	if len(spec.Columns) == 0 {
		return nil, errors.New("sortengine: spec has no columns")
	}
	if schema == nil {
		schema = NewSchema()
	}
	s := &Sorter{spec: spec}
	for i := range spec.Columns {
		c := spec.Columns[i]
		if c.Kind == KindNull || c.Kind > KindAny {
			return nil, fmt.Errorf("sortengine: column %q has invalid kind %d", c.Name, c.Kind)
		}
		if c.Pinyin && c.Kind != KindString && c.Kind != KindAny {
			return nil, fmt.Errorf("sortengine: column %q: Pinyin requires a string column", c.Name)
		}
		e := c.Expr
		if e == nil {
			idx, ok := schema.Index(c.Name)
			if !ok {
				return nil, fmt.Errorf("sortengine: column %q: no expression and no matching field", c.Name)
			}
			e = &FieldRef{Name: c.Name, Index: idx}
		}
		re, err := resolveExpr(e, schema)
		if err != nil {
			return nil, fmt.Errorf("sortengine: column %q: %w", c.Name, err)
		}
		s.cols = append(s.cols, compiledColumn{col: c, expr: re})
	}
	return s, nil
}

// Spec returns the compiled spec.
func (s *Sorter) Spec() Spec { return s.spec }

// ColumnDescriptions renders the spec for display.
func (s *Sorter) ColumnDescriptions() []ColumnDescription {
	out := make([]ColumnDescription, len(s.cols))
	for i, c := range s.cols {
		out[i] = ColumnDescription{
			Index:    i,
			Name:     c.col.Name,
			Kind:     c.col.Kind.String(),
			Expr:     c.expr.String(),
			Dir:      c.col.Dir.String(),
			Nulls:    c.col.Nulls.String(),
			Pinyin:   c.col.Pinyin,
			Encoding: encodingRule(c.col),
		}
	}
	return out
}

// ColumnDescription is the JSON-friendly view of one compiled sort column.
type ColumnDescription struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Expr     string `json:"expr"`
	Dir      string `json:"dir"`
	Nulls    string `json:"nulls"`
	Pinyin   bool   `json:"pinyin"`
	Encoding string `json:"encoding"`
}

func encodingRule(c Column) string {
	var b strings.Builder
	if c.Nulls == NullsFirst {
		b.WriteString("NULL 标记 0x00 / 非空标记 0x01; ")
	} else {
		b.WriteString("非空标记 0xfe / NULL 标记 0xff; ")
	}
	switch {
	case c.Kind == KindString && c.Pinyin:
		b.WriteString("每字 拼音码 2/3 字节，末尾 000000; ")
	case c.Kind == KindString:
		b.WriteString("UTF-8 字节，0x00 后补 0x01，末尾 00 00; ")
	case c.Kind == KindBool:
		b.WriteString("false→0x00, true→0x01; ")
	case c.Kind == KindInt:
		b.WriteString("int64 符号位取反后 8 字节大端; ")
	case c.Kind == KindUint:
		b.WriteString("uint64 8 字节大端; ")
	case c.Kind == KindFloat:
		b.WriteString("IEEE-754 保序映射 8 字节大端（-0 归一为 +0，NaN 在 +Inf 之后）; ")
	case c.Kind == KindAny:
		b.WriteString("类型标签 1..5 + 值字节; ")
	}
	if c.Dir == Desc {
		b.WriteString("降序：值字节按位取反")
	} else {
		b.WriteString("升序：直接按字节比较")
	}
	return b.String()
}
