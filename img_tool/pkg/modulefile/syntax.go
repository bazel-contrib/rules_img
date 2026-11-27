// Package modulefile parses Bazel MODULE.bazel files.
//
// MODULE.bazel is a restricted dialect of Starlark: a sequence of assignments
// and expression statements (mostly function calls), without load, def, if or
// for statements. That is all this parser accepts. It keeps the position of
// every expression, so callers can point tools like buildozer at the line a
// call starts on.
package modulefile

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Pos is a position in a file. Line and Col are 1-based.
type Pos struct {
	Line int
	Col  int
}

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

// Expr is an expression.
type Expr interface {
	// Start is where the expression starts.
	Start() Pos
}

// Ident is an identifier, including True, False and None.
type Ident struct {
	Pos  Pos
	Name string
}

// String is a string literal.
type String struct {
	Pos   Pos
	Value string
}

// Number is an integer or float literal, kept as written.
type Number struct {
	Pos Pos
	Raw string
}

// List is a list literal.
type List struct {
	Pos   Pos
	Elems []Expr
}

// Tuple is a tuple, with or without parentheses.
type Tuple struct {
	Pos   Pos
	Elems []Expr
}

// Dict is a dict literal.
type Dict struct {
	Pos     Pos
	Entries []DictEntry
}

// DictEntry is a key-value pair of a Dict.
type DictEntry struct {
	Key   Expr
	Value Expr
}

// Comprehension is a list or dict comprehension. Its clauses are parsed but
// not kept: comprehensions never evaluate to a known value.
type Comprehension struct {
	Pos Pos
}

// Dot is an attribute access, X.Name.
type Dot struct {
	X    Expr
	Name string
}

// Call is a function call. Its position is that of the called expression, so
// for images.pull(...) it is that of images.
type Call struct {
	Fn   Expr
	Args []Arg
}

// Arg is an argument of a Call. Name is empty for positional arguments. Star
// is 1 for *args and 2 for **kwargs.
type Arg struct {
	Name  string
	Value Expr
	Star  int
}

// Index is a subscript or slice, X[...].
type Index struct {
	X Expr
}

// Unary is a unary operation (-, +, ~, not).
type Unary struct {
	Pos Pos
	Op  string
	X   Expr
}

// Binary is a binary operation.
type Binary struct {
	Op string
	X  Expr
	Y  Expr
}

// Cond is a conditional expression, Then if Cond else Else.
type Cond struct {
	Then Expr
	Cond Expr
	Else Expr
}

func (e *Ident) Start() Pos         { return e.Pos }
func (e *String) Start() Pos        { return e.Pos }
func (e *Number) Start() Pos        { return e.Pos }
func (e *List) Start() Pos          { return e.Pos }
func (e *Tuple) Start() Pos         { return e.Pos }
func (e *Dict) Start() Pos          { return e.Pos }
func (e *Comprehension) Start() Pos { return e.Pos }
func (e *Dot) Start() Pos           { return e.X.Start() }
func (e *Call) Start() Pos          { return e.Fn.Start() }
func (e *Index) Start() Pos         { return e.X.Start() }
func (e *Unary) Start() Pos         { return e.Pos }
func (e *Binary) Start() Pos        { return e.X.Start() }
func (e *Cond) Start() Pos          { return e.Then.Start() }

// Keyword returns the value of the keyword argument name, or nil.
func (c *Call) Keyword(name string) Expr {
	for _, arg := range c.Args {
		if arg.Star == 0 && arg.Name == name {
			return arg.Value
		}
	}
	return nil
}

// Positional returns the positional arguments.
func (c *Call) Positional() []Expr {
	var positional []Expr
	for _, arg := range c.Args {
		if arg.Star == 0 && arg.Name == "" {
			positional = append(positional, arg.Value)
		}
	}
	return positional
}

// Stmt is a top-level statement: an *Assign or an *ExprStmt.
type Stmt interface {
	stmt()
}

// Assign is an assignment. Targets has more than one name for a, b = ...; Op
// is "=" or an augmented assignment operator such as "+=".
type Assign struct {
	Pos     Pos
	Targets []string
	Op      string
	Value   Expr
}

// ExprStmt is an expression used as a statement, typically a call.
type ExprStmt struct {
	X Expr
}

func (*Assign) stmt()   {}
func (*ExprStmt) stmt() {}

// File is a parsed MODULE.bazel file.
type File struct {
	Path  string
	Stmts []Stmt
}

// Error is a syntax error.
type Error struct {
	Path string
	Pos  Pos
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("%s:%s: %s", e.Path, e.Pos, e.Msg) }

// Parse parses the MODULE.bazel file src. path is only used in errors.
func Parse(path string, src []byte) (*File, error) {
	tokens, err := lex(path, string(src))
	if err != nil {
		return nil, err
	}
	p := &parser{path: path, tokens: tokens}
	file := &File{Path: path}
	for {
		for p.peek().kind == tokNewline || p.peek().is(";") {
			p.next()
		}
		if p.peek().kind == tokEOF {
			return file, nil
		}
		stmt, err := p.statement()
		if err != nil {
			return nil, err
		}
		file.Stmts = append(file.Stmts, stmt)
		if t := p.peek(); t.kind != tokNewline && t.kind != tokEOF && !t.is(";") {
			return nil, p.errorf(t, "unexpected %s after statement", t)
		}
	}
}

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokNewline
	tokIdent
	tokString
	tokNumber
	tokOp
)

type token struct {
	kind tokenKind
	pos  Pos
	text string // identifier, operator, raw number
	str  string // decoded string literal
}

func (t token) is(op string) bool { return t.kind == tokOp && t.text == op }

func (t token) isKeyword(keyword string) bool { return t.kind == tokIdent && t.text == keyword }

func (t token) String() string {
	switch t.kind {
	case tokEOF:
		return "end of file"
	case tokNewline:
		return "newline"
	case tokString:
		return "string " + strconv.Quote(t.str)
	default:
		return strconv.Quote(t.text)
	}
}

// operators, longest first so that the lexer matches greedily.
var operators = []string{
	"**=", "//=", "<<=", ">>=",
	"==", "!=", "<=", ">=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "//", "**", "<<", ">>", "->",
	"(", ")", "[", "]", "{", "}", ",", ":", ";", ".", "=", "<", ">", "+", "-", "*", "/", "%", "&", "|", "^", "~",
}

// lex splits src into tokens. Newlines are only significant outside of
// brackets, where they end a statement.
func lex(path, src string) ([]token, error) {
	var tokens []token
	line, col := 1, 1
	depth := 0
	i := 0
	advance := func(n int) {
		for _, r := range src[i : i+n] {
			if r == '\n' {
				line++
				col = 1
			} else {
				col++
			}
		}
		i += n
	}
	fail := func(pos Pos, format string, args ...any) error {
		return &Error{Path: path, Pos: pos, Msg: fmt.Sprintf(format, args...)}
	}
	for i < len(src) {
		c := src[i]
		pos := Pos{Line: line, Col: col}
		switch {
		case c == '\n':
			if depth == 0 && len(tokens) > 0 && tokens[len(tokens)-1].kind != tokNewline {
				tokens = append(tokens, token{kind: tokNewline, pos: pos})
			}
			advance(1)
		case c == ' ' || c == '\t' || c == '\r' || c == '\f':
			advance(1)
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				advance(1)
			}
		case c == '\\' && i+1 < len(src) && (src[i+1] == '\n' || (src[i+1] == '\r' && i+2 < len(src) && src[i+2] == '\n')):
			// Line continuation.
			if src[i+1] == '\r' {
				advance(3)
			} else {
				advance(2)
			}
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			word := src[i:j]
			if j < len(src) && (src[j] == '"' || src[j] == '\'') && isStringPrefix(word) {
				value, n, err := lexString(src[i:], len(word))
				if err != nil {
					return nil, fail(pos, "%v", err)
				}
				tokens = append(tokens, token{kind: tokString, pos: pos, str: value})
				advance(n)
				continue
			}
			tokens = append(tokens, token{kind: tokIdent, pos: pos, text: word})
			advance(j - i)
		case c == '"' || c == '\'':
			value, n, err := lexString(src[i:], 0)
			if err != nil {
				return nil, fail(pos, "%v", err)
			}
			tokens = append(tokens, token{kind: tokString, pos: pos, str: value})
			advance(n)
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			j := i
			for j < len(src) && (isIdentPart(src[j]) || src[j] == '.' ||
				((src[j] == '+' || src[j] == '-') && j > i && (src[j-1] == 'e' || src[j-1] == 'E') && !strings.HasPrefix(strings.ToLower(src[i:j]), "0x"))) {
				j++
			}
			tokens = append(tokens, token{kind: tokNumber, pos: pos, text: src[i:j]})
			advance(j - i)
		default:
			matched := ""
			for _, op := range operators {
				if strings.HasPrefix(src[i:], op) {
					matched = op
					break
				}
			}
			if matched == "" {
				r, _ := utf8.DecodeRuneInString(src[i:])
				return nil, fail(pos, "unexpected character %q", r)
			}
			switch matched {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				if depth == 0 {
					return nil, fail(pos, "unbalanced %q", matched)
				}
				depth--
			}
			tokens = append(tokens, token{kind: tokOp, pos: pos, text: matched})
			advance(len(matched))
		}
	}
	if depth != 0 {
		return nil, fail(Pos{Line: line, Col: col}, "unexpected end of file inside brackets")
	}
	if len(tokens) > 0 && tokens[len(tokens)-1].kind != tokNewline {
		tokens = append(tokens, token{kind: tokNewline, pos: Pos{Line: line, Col: col}})
	}
	return append(tokens, token{kind: tokEOF, pos: Pos{Line: line, Col: col}}), nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= utf8.RuneSelf
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isStringPrefix(word string) bool {
	switch strings.ToLower(word) {
	case "r", "b", "rb", "br":
		return true
	}
	return false
}

// lexString decodes the string literal at the start of s, after a prefix of
// prefixLen bytes (r, b, rb). It returns the value and the length of the
// literal including the prefix.
func lexString(s string, prefixLen int) (string, int, error) {
	raw := strings.ContainsAny(s[:prefixLen], "rR")
	i := prefixLen
	quote := s[i]
	delim := string(quote)
	if strings.HasPrefix(s[i:], strings.Repeat(string(quote), 3)) {
		delim = strings.Repeat(string(quote), 3)
	}
	i += len(delim)
	var b strings.Builder
	for {
		if i >= len(s) {
			return "", 0, fmt.Errorf("unterminated string")
		}
		if strings.HasPrefix(s[i:], delim) {
			return b.String(), i + len(delim), nil
		}
		c := s[i]
		if c == '\n' && len(delim) == 1 {
			return "", 0, fmt.Errorf("unterminated string")
		}
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			return "", 0, fmt.Errorf("unterminated string")
		}
		next := s[i+1]
		if raw {
			// In raw strings, a backslash escapes nothing, but still keeps
			// the next character from ending the string.
			b.WriteByte('\\')
			b.WriteByte(next)
			i += 2
			continue
		}
		i += 2
		switch next {
		case '\n':
			// Escaped newline: dropped.
		case '\\', '\'', '"':
			b.WriteByte(next)
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case 'x', 'u', 'U':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[next]
			if i+n > len(s) {
				return "", 0, fmt.Errorf("truncated \\%c escape", next)
			}
			value, err := strconv.ParseUint(s[i:i+n], 16, 32)
			if err != nil {
				return "", 0, fmt.Errorf("invalid \\%c escape", next)
			}
			if next == 'x' {
				b.WriteByte(byte(value))
			} else {
				b.WriteRune(rune(value))
			}
			i += n
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i - 1
			for j < len(s) && j < i+2 && s[j] >= '0' && s[j] <= '7' {
				j++
			}
			value, _ := strconv.ParseUint(s[i-1:j], 8, 8)
			b.WriteByte(byte(value))
			i = j
		default:
			// Starlark rejects unknown escapes; keep them verbatim, the
			// parser is not the place to enforce that.
			b.WriteByte('\\')
			b.WriteByte(next)
		}
	}
}

type parser struct {
	path   string
	tokens []token
	i      int
}

func (p *parser) peek() token { return p.tokens[p.i] }

func (p *parser) next() token {
	t := p.tokens[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

func (p *parser) errorf(t token, format string, args ...any) error {
	return &Error{Path: p.path, Pos: t.pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) expect(op string) (token, error) {
	t := p.next()
	if !t.is(op) {
		return t, p.errorf(t, "expected %q, got %s", op, t)
	}
	return t, nil
}

var assignOps = map[string]bool{"=": true, "+=": true, "-=": true, "*=": true, "/=": true, "//=": true, "%=": true, "&=": true, "|=": true, "^=": true, "<<=": true, ">>=": true, "**=": true}

var statementKeywords = map[string]bool{"load": false, "def": true, "if": true, "for": true, "while": true, "return": true, "pass": true, "break": true, "continue": true, "lambda": true}

func (p *parser) statement() (Stmt, error) {
	start := p.peek()
	if start.kind == tokIdent && statementKeywords[start.text] {
		return nil, p.errorf(start, "%q statements are not allowed in MODULE.bazel files", start.text)
	}
	x, err := p.exprList()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind != tokOp || !assignOps[t.text] {
		return &ExprStmt{X: x}, nil
	}
	p.next()
	targets, err := assignTargets(x)
	if err != nil {
		return nil, p.errorf(start, "%v", err)
	}
	value, err := p.exprList()
	if err != nil {
		return nil, err
	}
	return &Assign{Pos: start.pos, Targets: targets, Op: t.text, Value: value}, nil
}

func assignTargets(x Expr) ([]string, error) {
	switch x := x.(type) {
	case *Ident:
		return []string{x.Name}, nil
	case *Tuple:
		var names []string
		for _, elem := range x.Elems {
			ident, ok := elem.(*Ident)
			if !ok {
				return nil, fmt.Errorf("can only assign to identifiers")
			}
			names = append(names, ident.Name)
		}
		return names, nil
	case *List:
		return assignTargets(&Tuple{Pos: x.Pos, Elems: x.Elems})
	}
	return nil, fmt.Errorf("can only assign to identifiers")
}

// exprList parses an expression, or a tuple of them without parentheses.
func (p *parser) exprList() (Expr, error) {
	start := p.peek()
	x, err := p.test()
	if err != nil {
		return nil, err
	}
	if !p.peek().is(",") {
		return x, nil
	}
	elems := []Expr{x}
	for p.peek().is(",") {
		p.next()
		if !p.startsExpr() {
			break
		}
		elem, err := p.test()
		if err != nil {
			return nil, err
		}
		elems = append(elems, elem)
	}
	return &Tuple{Pos: start.pos, Elems: elems}, nil
}

func (p *parser) startsExpr() bool {
	t := p.peek()
	switch t.kind {
	case tokIdent:
		return !(t.text == "for" || t.text == "if" || t.text == "else" || t.text == "in")
	case tokString, tokNumber:
		return true
	case tokOp:
		switch t.text {
		case "(", "[", "{", "-", "+", "~":
			return true
		}
	}
	return false
}

// test parses a conditional expression or anything of higher precedence.
func (p *parser) test() (Expr, error) {
	if t := p.peek(); t.isKeyword("lambda") {
		return nil, p.errorf(t, "lambda expressions are not supported")
	}
	x, err := p.binary(0)
	if err != nil {
		return nil, err
	}
	if !p.peek().isKeyword("if") {
		return x, nil
	}
	p.next()
	cond, err := p.binary(0)
	if err != nil {
		return nil, err
	}
	if t := p.next(); !t.isKeyword("else") {
		return nil, p.errorf(t, "expected else, got %s", t)
	}
	otherwise, err := p.test()
	if err != nil {
		return nil, err
	}
	return &Cond{Then: x, Cond: cond, Else: otherwise}, nil
}

// precedence of binary operators, lowest first.
var precedence = map[string]int{
	"or":  1,
	"and": 2,
	// 3 is "not", a unary operator.
	"==": 4, "!=": 4, "<": 4, ">": 4, "<=": 4, ">=": 4, "in": 4, "not in": 4,
	"|":  5,
	"^":  6,
	"&":  7,
	"<<": 8, ">>": 8,
	"+": 9, "-": 9,
	"*": 10, "/": 10, "//": 10, "%": 10,
}

func (p *parser) binaryOp() (string, int) {
	t := p.peek()
	switch {
	case t.kind == tokOp:
		if prec, ok := precedence[t.text]; ok {
			return t.text, prec
		}
	case t.kind == tokIdent && (t.text == "or" || t.text == "and" || t.text == "in"):
		return t.text, precedence[t.text]
	case t.isKeyword("not") && p.tokens[p.i+1].isKeyword("in"):
		return "not in", precedence["not in"]
	}
	return "", -1
}

func (p *parser) binary(minPrec int) (Expr, error) {
	x, err := p.unary(minPrec)
	if err != nil {
		return nil, err
	}
	for {
		op, prec := p.binaryOp()
		if prec < 0 || prec <= minPrec {
			return x, nil
		}
		p.next()
		if op == "not in" {
			p.next()
		}
		y, err := p.binary(prec)
		if err != nil {
			return nil, err
		}
		x = &Binary{Op: op, X: x, Y: y}
	}
}

func (p *parser) unary(minPrec int) (Expr, error) {
	t := p.peek()
	if t.isKeyword("not") && minPrec < 3 {
		p.next()
		x, err := p.binary(3)
		if err != nil {
			return nil, err
		}
		return &Unary{Pos: t.pos, Op: "not", X: x}, nil
	}
	if t.is("-") || t.is("+") || t.is("~") {
		p.next()
		x, err := p.unary(precedence["*"])
		if err != nil {
			return nil, err
		}
		return &Unary{Pos: t.pos, Op: t.text, X: x}, nil
	}
	return p.primary()
}

func (p *parser) primary() (Expr, error) {
	x, err := p.operand()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case t.is("."):
			p.next()
			name := p.next()
			if name.kind != tokIdent {
				return nil, p.errorf(name, "expected attribute name, got %s", name)
			}
			x = &Dot{X: x, Name: name.text}
		case t.is("("):
			p.next()
			args, err := p.arguments()
			if err != nil {
				return nil, err
			}
			x = &Call{Fn: x, Args: args}
		case t.is("["):
			p.next()
			if err := p.skipUntilClose("]"); err != nil {
				return nil, err
			}
			x = &Index{X: x}
		default:
			return x, nil
		}
	}
}

// skipUntilClose skips the tokens up to and including the bracket close that
// matches an opening bracket the caller already consumed.
func (p *parser) skipUntilClose(close string) error {
	depth := 1
	for {
		t := p.next()
		switch {
		case t.kind == tokEOF:
			return p.errorf(t, "expected %q", close)
		case t.is("(") || t.is("[") || t.is("{"):
			depth++
		case t.is(")") || t.is("]") || t.is("}"):
			depth--
			if depth == 0 {
				if t.text != close {
					return p.errorf(t, "expected %q, got %s", close, t)
				}
				return nil
			}
		}
	}
}

func (p *parser) arguments() ([]Arg, error) {
	var args []Arg
	for !p.peek().is(")") {
		var arg Arg
		switch t := p.peek(); {
		case t.is("**"):
			p.next()
			arg.Star = 2
		case t.is("*"):
			p.next()
			arg.Star = 1
		case t.kind == tokIdent && p.tokens[p.i+1].is("="):
			p.next()
			p.next()
			arg.Name = t.text
		}
		value, err := p.test()
		if err != nil {
			return nil, err
		}
		arg.Value = value
		args = append(args, arg)
		if p.peek().is(",") {
			p.next()
			continue
		}
		if !p.peek().is(")") {
			t := p.peek()
			return nil, p.errorf(t, "expected \",\" or \")\" in arguments, got %s", t)
		}
	}
	p.next()
	return args, nil
}

func (p *parser) operand() (Expr, error) {
	t := p.next()
	switch t.kind {
	case tokIdent:
		if t.text == "for" || t.text == "if" || t.text == "else" || t.text == "in" || t.text == "lambda" {
			return nil, p.errorf(t, "unexpected keyword %q", t.text)
		}
		return &Ident{Pos: t.pos, Name: t.text}, nil
	case tokString:
		return &String{Pos: t.pos, Value: t.str}, nil
	case tokNumber:
		return &Number{Pos: t.pos, Raw: t.text}, nil
	case tokOp:
		switch t.text {
		case "(":
			if p.peek().is(")") {
				p.next()
				return &Tuple{Pos: t.pos}, nil
			}
			x, err := p.exprList()
			if err != nil {
				return nil, err
			}
			if p.peek().isKeyword("for") {
				if err := p.skipUntilClose(")"); err != nil {
					return nil, err
				}
				return &Comprehension{Pos: t.pos}, nil
			}
			if _, err := p.expect(")"); err != nil {
				return nil, err
			}
			if tuple, ok := x.(*Tuple); ok {
				tuple.Pos = t.pos
			}
			return x, nil
		case "[":
			return p.list(t)
		case "{":
			return p.dict(t)
		}
	}
	return nil, p.errorf(t, "unexpected %s", t)
}

func (p *parser) list(open token) (Expr, error) {
	list := &List{Pos: open.pos}
	for !p.peek().is("]") {
		elem, err := p.test()
		if err != nil {
			return nil, err
		}
		if p.peek().isKeyword("for") {
			if err := p.skipUntilClose("]"); err != nil {
				return nil, err
			}
			return &Comprehension{Pos: open.pos}, nil
		}
		list.Elems = append(list.Elems, elem)
		if p.peek().is(",") {
			p.next()
			continue
		}
		if !p.peek().is("]") {
			t := p.peek()
			return nil, p.errorf(t, "expected \",\" or \"]\" in list, got %s", t)
		}
	}
	p.next()
	return list, nil
}

func (p *parser) dict(open token) (Expr, error) {
	dict := &Dict{Pos: open.pos}
	for !p.peek().is("}") {
		key, err := p.test()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(":"); err != nil {
			return nil, err
		}
		value, err := p.test()
		if err != nil {
			return nil, err
		}
		if p.peek().isKeyword("for") {
			if err := p.skipUntilClose("}"); err != nil {
				return nil, err
			}
			return &Comprehension{Pos: open.pos}, nil
		}
		dict.Entries = append(dict.Entries, DictEntry{Key: key, Value: value})
		if p.peek().is(",") {
			p.next()
			continue
		}
		if !p.peek().is("}") {
			t := p.peek()
			return nil, p.errorf(t, "expected \",\" or \"}\" in dict, got %s", t)
		}
	}
	p.next()
	return dict, nil
}
