package sortengine

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Expr is an evaluated-once-per-row sort key expression. A NULL operand makes
// an expression NULL (except for the three-valued AND/OR and the functions
// documented below), and division by zero is NULL so that a row with a zero
// duration sorts with the missing durations instead of failing the run.
type Expr interface {
	Eval(row Row) Value
	String() string
}

// FieldRef reads a field of the input row by position.
type FieldRef struct {
	Name  string
	Index int
}

func (f *FieldRef) Eval(row Row) Value {
	if f.Index < 0 || f.Index >= len(row) {
		return Null
	}
	return row[f.Index]
}

func (f *FieldRef) String() string { return f.Name }

// LiteralExpr is a constant in an expression.
type LiteralExpr struct{ V Value }

func (l *LiteralExpr) Eval(Row) Value { return l.V }
func (l *LiteralExpr) String() string {
	switch l.V.Kind {
	case KindString:
		return quoteString(l.V.Str)
	case KindNull:
		return "NULL"
	default:
		return l.V.String()
	}
}

func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// UnaryExpr is -, + or NOT.
type UnaryExpr struct {
	Op string
	X  Expr
}

func (u *UnaryExpr) Eval(row Row) Value {
	v := u.X.Eval(row)
	switch u.Op {
	case "-":
		switch v.Kind {
		case KindNull:
			return Null
		case KindInt:
			return Int(-v.Int)
		case KindUint:
			return Float(-float64(v.Uint))
		case KindFloat:
			return Float(-math.Float64frombits(v.Float))
		}
		return Null
	case "+":
		if isNumeric(v.Kind) {
			return v
		}
		return Null
	case "NOT":
		if v.Kind == KindNull {
			return Null
		}
		if v.Kind == KindBool {
			return Bool(!v.Bool)
		}
		return Null
	}
	return Null
}

func (u *UnaryExpr) String() string {
	if u.Op == "NOT" {
		return "(NOT " + u.X.String() + ")"
	}
	return u.Op + u.X.String()
}

// BinaryExpr is an arithmetic, comparison or logical operator.
type BinaryExpr struct {
	Op   string
	L, R Expr
}

func (b *BinaryExpr) String() string {
	return "(" + b.L.String() + " " + b.Op + " " + b.R.String() + ")"
}

func (b *BinaryExpr) Eval(row Row) Value {
	l := b.L.Eval(row)
	r := b.R.Eval(row)
	switch b.Op {
	case "AND", "OR":
		return evalLogic(b.Op, l, r)
	case "=", "==", "!=", "<>", "<", "<=", ">", ">=":
		if l.IsNull() || r.IsNull() {
			return Null
		}
		c, ok := CompareValues(l, r)
		if !ok {
			return Null
		}
		var res bool
		switch b.Op {
		case "=", "==":
			res = c == 0
		case "!=", "<>":
			res = c != 0
		case "<":
			res = c < 0
		case "<=":
			res = c <= 0
		case ">":
			res = c > 0
		case ">=":
			res = c >= 0
		}
		return Bool(res)
	}
	if l.IsNull() || r.IsNull() {
		return Null
	}
	return arith(b.Op, l, r)
}

func evalLogic(op string, l, r Value) Value {
	if l.Kind != KindNull && l.Kind != KindBool {
		return Null
	}
	if r.Kind != KindNull && r.Kind != KindBool {
		return Null
	}
	a, aok := boolOf(l)
	b, bok := boolOf(r)
	if op == "AND" {
		if aok && !a || bok && !b {
			return Bool(false)
		}
		if !aok || !bok {
			return Null
		}
		return Bool(true)
	}
	// OR
	if aok && a || bok && b {
		return Bool(true)
	}
	if !aok || !bok {
		return Null
	}
	return Bool(false)
}

func boolOf(v Value) (bool, bool) {
	if v.Kind == KindNull {
		return false, false
	}
	return v.Bool, true
}

func isNumeric(k Kind) bool { return k == KindInt || k == KindUint || k == KindFloat }

// numericRank: bool=0 int/uint=1 float=2.
func arith(op string, l, r Value) Value {
	if l.Kind == KindBool || r.Kind == KindBool {
		return Null
	}
	if !isNumeric(l.Kind) || !isNumeric(r.Kind) {
		return Null
	}
	if op == "/" {
		rf := toFloat(r)
		if rf == 0 {
			return Null // division by zero is treated as undefined -> NULL
		}
		return Float(toFloat(l) / rf)
	}
	if op == "%" {
		if l.Kind == KindFloat || r.Kind == KindFloat {
			return Null // modulo is defined for integers only
		}
		if r.Kind == KindInt {
			if r.Int == 0 {
				return Null
			}
			if l.Kind == KindInt {
				return Int(l.Int % r.Int)
			}
			return Float(math.Mod(toFloat(l), toFloat(r)))
		}
		if r.Uint == 0 {
			return Null
		}
		if l.Kind == KindUint {
			return Uint(l.Uint % r.Uint)
		}
		return Float(math.Mod(toFloat(l), toFloat(r)))
	}
	// Integer fast path when both sides are the same integer kind.
	if l.Kind == KindInt && r.Kind == KindInt {
		switch op {
		case "+":
			return Int(l.Int + r.Int)
		case "-":
			return Int(l.Int - r.Int)
		case "*":
			return Int(l.Int * r.Int)
		}
	}
	if l.Kind == KindUint && r.Kind == KindUint {
		switch op {
		case "+":
			return Uint(l.Uint + r.Uint)
		case "-":
			return Uint(l.Uint - r.Uint)
		case "*":
			return Uint(l.Uint * r.Uint)
		}
	}
	a, b := toFloat(l), toFloat(r)
	switch op {
	case "+":
		return Float(a + b)
	case "-":
		return Float(a - b)
	case "*":
		return Float(a * b)
	}
	return Null
}

func toFloat(v Value) float64 {
	switch v.Kind {
	case KindInt:
		return float64(v.Int)
	case KindUint:
		return float64(v.Uint)
	case KindFloat:
		return math.Float64frombits(v.Float)
	case KindBool:
		if v.Bool {
			return 1
		}
		return 0
	}
	return math.NaN()
}

// CallExpr is a built-in function call.
type CallExpr struct {
	Name string
	Args []Expr
}

func (c *CallExpr) String() string {
	parts := make([]string, len(c.Args))
	for i, a := range c.Args {
		parts[i] = a.String()
	}
	return c.Name + "(" + strings.Join(parts, ", ") + ")"
}

func (c *CallExpr) Eval(row Row) Value {
	switch c.Name {
	case "coalesce":
		for _, a := range c.Args {
			if v := a.Eval(row); !v.IsNull() {
				return v
			}
		}
		return Null
	case "nullif":
		a := c.Args[0].Eval(row)
		b := c.Args[1].Eval(row)
		if a.IsNull() {
			return Null
		}
		if b.IsNull() {
			return a
		}
		if cmp, ok := CompareValues(a, b); ok && cmp == 0 {
			return Null
		}
		return a
	case "ifnull":
		if v := c.Args[0].Eval(row); !v.IsNull() {
			return v
		}
		return c.Args[1].Eval(row)
	case "abs":
		v := c.Args[0].Eval(row)
		switch v.Kind {
		case KindNull:
			return Null
		case KindInt:
			if v.Int < 0 {
				if v.Int == math.MinInt64 {
					return Int(v.Int)
				}
				return Int(-v.Int)
			}
			return v
		case KindUint:
			return v
		case KindFloat:
			return Float(math.Abs(math.Float64frombits(v.Float)))
		}
		return Null
	case "floor", "ceil":
		v := c.Args[0].Eval(row)
		if v.Kind == KindNull {
			return Null
		}
		if !isNumeric(v.Kind) {
			return Null
		}
		f := toFloat(v)
		if c.Name == "floor" {
			return Float(math.Floor(f))
		}
		return Float(math.Ceil(f))
	case "round":
		v := c.Args[0].Eval(row)
		if v.Kind == KindNull || !isNumeric(v.Kind) {
			return Null
		}
		digits := 0
		if len(c.Args) > 1 {
			d := c.Args[1].Eval(row)
			if d.Kind != KindInt {
				return Null
			}
			digits = int(d.Int)
		}
		p := math.Pow(10, float64(digits))
		return Float(math.Round(toFloat(v)*p) / p)
	}
	return Null
}

// parseExpr parses the expression dialect used by sort columns.
func parseExpr(src string) (Expr, error) {
	p := &parser{src: src}
	e, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos < len(p.src) {
		return nil, fmt.Errorf("unexpected %q at offset %d", p.src[p.pos:], p.pos)
	}
	return e, nil
}

type parser struct {
	src string
	pos int
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) {
		r, w := rune(p.src[p.pos]), 1
		if r >= 0x80 {
			r, w = decodeRune(p.src[p.pos:])
		}
		if !unicode.IsSpace(r) {
			return
		}
		p.pos += w
	}
}

func decodeRune(s string) (rune, int) {
	for i := 1; i <= len(s) && i <= 4; i++ {
		r := []rune(s[:i])
		if len(r) == 1 && r[0] != unicode.ReplacementChar {
			return r[0], i
		}
	}
	return rune(s[0]), 1
}

func (p *parser) peekWord() string {
	p.skipSpace()
	start := p.pos
	pos := p.pos
	for pos < len(p.src) {
		r, w := decodeRune(p.src[pos:])
		if !isIdentPart(r) {
			break
		}
		pos += w
	}
	return p.src[start:pos]
}

func (p *parser) consumeWord(w string) bool {
	p.skipSpace()
	if len(p.src)-p.pos >= len(w) && strings.EqualFold(p.src[p.pos:p.pos+len(w)], w) {
		rest := p.pos + len(w)
		if rest == len(p.src) {
			p.pos = rest
			return true
		}
		r, _ := decodeRune(p.src[rest:])
		if !isIdentPart(r) {
			p.pos = rest
			return true
		}
	}
	return false
}

func isIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func isIdentPart(r rune) bool  { return isIdentStart(r) || unicode.IsDigit(r) }

func (p *parser) parseOr() (Expr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.consumeWord("OR") || p.consumeWord("or") || p.consumeSymbol("||") {
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &BinaryExpr{Op: "OR", L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseAnd() (Expr, error) {
	l, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.consumeWord("AND") || p.consumeWord("and") || p.consumeSymbol("&&") {
		r, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l = &BinaryExpr{Op: "AND", L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseNot() (Expr, error) {
	if p.consumeWord("NOT") {
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "NOT", X: x}, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (Expr, error) {
	l, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.consumeSymbol("<="):
			op = "<="
		case p.consumeSymbol(">="):
			op = ">="
		case p.consumeSymbol("<>"):
			op = "!="
		case p.consumeSymbol("!="):
			op = "!="
		case p.consumeSymbol("=="):
			op = "="
		case p.consumeSymbol("="):
			op = "="
		case p.consumeSymbol("<"):
			op = "<"
		case p.consumeSymbol(">"):
			op = ">"
		default:
			return l, nil
		}
		r, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		l = &BinaryExpr{Op: op, L: l, R: r}
	}
}

func (p *parser) parseAdditive() (Expr, error) {
	l, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.consumeSymbol("+"):
			op = "+"
		case p.consumeSymbol("-"):
			op = "-"
		default:
			return l, nil
		}
		r, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		l = &BinaryExpr{Op: op, L: l, R: r}
	}
}

func (p *parser) parseMultiplicative() (Expr, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.consumeSymbol("*"):
			op = "*"
		case p.consumeSymbol("/"):
			op = "/"
		case p.consumeSymbol("%"):
			op = "%"
		default:
			return l, nil
		}
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &BinaryExpr{Op: op, L: l, R: r}
	}
}

func (p *parser) parseUnary() (Expr, error) {
	if p.consumeSymbol("-") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "-", X: x}, nil
	}
	if p.consumeSymbol("+") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "+", X: x}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, errors.New("unexpected end of expression")
	}
	if p.consumeSymbol("(") {
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.consumeSymbol(")") {
			return nil, errors.New("missing closing parenthesis")
		}
		return e, nil
	}
	if p.src[p.pos] == '\'' || p.src[p.pos] == '"' {
		return p.parseString()
	}
	// number
	if c := p.src[p.pos]; c >= '0' && c <= '9' || (c == '.' && p.pos+1 < len(p.src) && p.src[p.pos+1] >= '0' && p.src[p.pos+1] <= '9') {
		return p.parseNumber()
	}
	if !isIdentStart(rune(p.src[p.pos])) {
		r, _ := decodeRune(p.src[p.pos:])
		if !isIdentStart(r) {
			return nil, fmt.Errorf("unexpected %q at offset %d", r, p.pos)
		}
	}
	word := p.peekWord()
	if word == "" {
		return nil, fmt.Errorf("expected identifier at offset %d", p.pos)
	}
	p.pos += len(word)
	switch strings.ToLower(word) {
	case "true":
		return &LiteralExpr{V: Bool(true)}, nil
	case "false":
		return &LiteralExpr{V: Bool(false)}, nil
	case "null":
		return &LiteralExpr{V: Null}, nil
	}
	if p.consumeSymbol("(") {
		var args []Expr
		if !p.consumeSymbol(")") {
			for {
				a, err := p.parseOr()
				if err != nil {
					return nil, err
				}
				args = append(args, a)
				if p.consumeSymbol(")") {
					break
				}
				if !p.consumeSymbol(",") {
					return nil, fmt.Errorf("expected , or ) after argument at offset %d", p.pos)
				}
			}
		}
		name := strings.ToLower(word)
		if err := checkArity(name, len(args)); err != nil {
			return nil, err
		}
		return &CallExpr{Name: name, Args: args}, nil
	}
	return &FieldRef{Name: word, Index: -1}, nil
}

func checkArity(name string, n int) error {
	min, max := 0, 0
	switch name {
	case "coalesce":
		return nil // any arity >= 1
	case "nullif", "ifnull":
		min, max = 2, 2
	case "abs", "floor", "ceil":
		min, max = 1, 1
	case "round":
		min, max = 1, 2
	default:
		return fmt.Errorf("unknown function %q", name)
	}
	if n < min || n > max {
		return fmt.Errorf("function %s takes %d..%d arguments, got %d", name, min, max, n)
	}
	return nil
}

func (p *parser) parseString() (Expr, error) {
	q := p.src[p.pos]
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == q {
			if p.pos+1 < len(p.src) && p.src[p.pos+1] == q { // doubled quote
				b.WriteByte(q)
				p.pos += 2
				continue
			}
			p.pos++
			return &LiteralExpr{V: Str(b.String())}, nil
		}
		if c == '\\' && p.pos+1 < len(p.src) {
			p.pos++
			switch p.src[p.pos] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '0':
				b.WriteByte(0)
			default:
				b.WriteByte(p.src[p.pos])
			}
			p.pos++
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return nil, errors.New("unterminated string literal")
}

func (p *parser) parseNumber() (Expr, error) {
	p.skipSpace()
	start := p.pos
	isFloat := false
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c >= '0' && c <= '9' {
			p.pos++
			continue
		}
		if c == '.' && !isFloat {
			isFloat = true
			p.pos++
			continue
		}
		break
	}
	if p.pos < len(p.src) && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		isFloat = true
		p.pos++
		if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
			p.pos++
		}
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
	}
	lit := p.src[start:p.pos]
	if isFloat {
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			return nil, fmt.Errorf("bad number %q", lit)
		}
		return &LiteralExpr{V: Float(f)}, nil
	}
	n, err := strconv.ParseInt(lit, 10, 64)
	if err != nil {
		u, err2 := strconv.ParseUint(lit, 10, 64)
		if err2 != nil {
			return nil, fmt.Errorf("bad integer %q", lit)
		}
		return &LiteralExpr{V: Uint(u)}, nil
	}
	return &LiteralExpr{V: Int(n)}, nil
}

func (p *parser) consumeSymbol(s string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.src[p.pos:], s) {
		// Do not treat the "<" of "<=" as a standalone comparison.
		if s == "<" || s == ">" {
			if strings.HasPrefix(p.src[p.pos:], s+"=") || strings.HasPrefix(p.src[p.pos:], "<>") {
				return false
			}
		}
		p.pos += len(s)
		return true
	}
	return false
}

func resolveExpr(e Expr, schema *Schema) (Expr, error) {
	switch x := e.(type) {
	case *FieldRef:
		if x.Index >= 0 {
			return x, nil
		}
		idx, ok := schema.Index(x.Name)
		if !ok {
			return nil, fmt.Errorf("unknown field %q", x.Name)
		}
		return &FieldRef{Name: x.Name, Index: idx}, nil
	case *LiteralExpr:
		return x, nil
	case *UnaryExpr:
		r, err := resolveExpr(x.X, schema)
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: x.Op, X: r}, nil
	case *BinaryExpr:
		l, err := resolveExpr(x.L, schema)
		if err != nil {
			return nil, err
		}
		r, err := resolveExpr(x.R, schema)
		if err != nil {
			return nil, err
		}
		return &BinaryExpr{Op: x.Op, L: l, R: r}, nil
	case *CallExpr:
		args := make([]Expr, len(x.Args))
		for i, a := range x.Args {
			r, err := resolveExpr(a, schema)
			if err != nil {
				return nil, err
			}
			args[i] = r
		}
		return &CallExpr{Name: x.Name, Args: args}, nil
	}
	return nil, fmt.Errorf("unsupported expression %T", e)
}

// ParseColumn parses one column definition from an expression string, used by
// the demo server and the verification program.
func ParseColumn(name, kindName, exprSrc string, dir Direction, nulls NullsOrder, pinyin bool) (Column, error) {
	k, ok := ParseKind(kindName)
	if !ok {
		return Column{}, fmt.Errorf("unknown kind %q", kindName)
	}
	e, err := parseExpr(exprSrc)
	if err != nil {
		return Column{}, fmt.Errorf("column %q: %w", name, err)
	}
	return Column{Name: name, Kind: k, Expr: e, Dir: dir, Nulls: nulls, Pinyin: pinyin}, nil
}

func ParseKind(s string) (Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "any":
		return KindAny, true
	case "bool", "boolean":
		return KindBool, true
	case "int", "int64", "integer":
		return KindInt, true
	case "uint", "uint64":
		return KindUint, true
	case "float", "float64", "double":
		return KindFloat, true
	case "string", "text":
		return KindString, true
	}
	return KindNull, false
}

// ParseDirection parses asc/desc case-insensitively.
func ParseDirection(s string) Direction {
	if strings.EqualFold(strings.TrimSpace(s), "desc") {
		return Desc
	}
	return Asc
}

// ParseNullsOrder parses first/last (also "nulls first"/"nulls last").
func ParseNullsOrder(s string) NullsOrder {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "nulls")
	s = strings.TrimSpace(s)
	if s == "last" || s == "nulls last" {
		return NullsLast
	}
	return NullsFirst
}
