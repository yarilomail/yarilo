// Package expand is the %{...} template language of configuration values:
// variables, providers such as env: and filters chained with |.
package expand

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxProgramSize bounds a template, as the reference does.
const maxProgramSize = 8192

type paramType int

const (
	paramString paramType = iota
	paramInt
	paramVariable
)

// param is one filter argument; idx is -1 for a named one.
type param struct {
	idx int
	key string
	typ paramType
	str string
	num int64
}

type statement struct {
	function string
	params   []param
}

type program struct {
	stmts       []statement
	onlyLiteral bool
}

// Program is a parsed template.
type Program struct {
	programs []*program
}

type tokenKind int

const (
	tEOF tokenKind = iota
	tValue
	tPerc
	tOCBrace
	tCCBrace
	tPipe
	tOBrace
	tCBrace
	tComma
	tEq
	tPlus
	tMinus
	tStar
	tSlash
	tName
	tNumber
)

var tokenNames = map[tokenKind]string{
	tEOF: "end of file", tValue: "VALUE", tPerc: "PERC", tOCBrace: "OCBRACE", tCCBrace: "CCBRACE",
	tPipe: "PIPE", tOBrace: "OBRACE", tCBrace: "CBRACE", tComma: "COMMA", tEq: "EQ",
	tPlus: "PLUS", tMinus: "MINUS", tStar: "STAR", tSlash: "SLASH", tName: "NAME", tNumber: "NUMBER",
}

type token struct {
	kind tokenKind
	text string
}

// lex splits a template into tokens: text and % outside braces, the filter
// language inside them.
func lex(s string) ([]token, error) {
	var out []token
	inExpr := false
	for i := 0; i < len(s); {
		if !inExpr {
			switch {
			case strings.HasPrefix(s[i:], "%%{"):
				out = append(out, token{tValue, "%{"})
				i += 3
			case strings.HasPrefix(s[i:], "%{"):
				out = append(out, token{kind: tOCBrace})
				inExpr = true
				i += 2
			case s[i] == '%':
				out = append(out, token{kind: tPerc})
				i++
			default:
				j := strings.IndexByte(s[i:], '%')
				if j < 0 {
					j = len(s) - i
				}
				out = append(out, token{tValue, s[i : i+j]})
				i += j
			}
			continue
		}
		c := s[i]
		switch {
		case c == '}':
			out = append(out, token{kind: tCCBrace})
			inExpr = false
			i++
		case c == ' ' || c == '\t':
			i++
		case c == '%':
			out = append(out, token{kind: tPerc})
			i++
		case c == '|':
			out = append(out, token{kind: tPipe})
			i++
		case c == '(':
			out = append(out, token{kind: tOBrace})
			i++
		case c == ')':
			out = append(out, token{kind: tCBrace})
			i++
		case c == ',':
			out = append(out, token{kind: tComma})
			i++
		case c == '=':
			out = append(out, token{kind: tEq})
			i++
		case c == '+':
			out = append(out, token{kind: tPlus})
			i++
		case c == '-':
			out = append(out, token{kind: tMinus})
			i++
		case c == '*':
			out = append(out, token{kind: tStar})
			i++
		case c == '/':
			out = append(out, token{kind: tSlash})
			i++
		case c == '\'' || c == '"':
			str, n, err := lexString(s[i+1:], c)
			if err != nil {
				return nil, err
			}
			out = append(out, token{tValue, str})
			i += n + 1
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			out = append(out, token{tNumber, s[i:j]})
			i = j
		case isNameStart(c):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			if !utf8.ValidString(s[i:j]) {
				return nil, errors.New("Invalid UTF-8 string")
			}
			out = append(out, token{tName, s[i:j]})
			i = j
		default:
			return nil, errors.New("Premature program end")
		}
	}
	out = append(out, token{kind: tEOF})
	return out, nil
}

func isNameStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isNameChar(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9' || strings.IndexByte("_/;:.-", c) >= 0
}

// lexString reads a quoted string after its opening quote; n counts the
// bytes consumed, the closing quote included.
func lexString(s string, quote byte) (string, int, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == quote {
			if !utf8.ValidString(b.String()) {
				return "", 0, errors.New("Invalid UTF-8 string")
			}
			return b.String(), i + 1, nil
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		switch e := s[i]; {
		case e == '\\':
			b.WriteByte('\\')
		case e == 't':
			b.WriteByte('\t')
		case e == 'r':
			b.WriteByte('\r')
		case e == 'n':
			b.WriteByte('\n')
		case e == '"' || e == '\'':
			b.WriteByte(e)
		case e == 'x':
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return "", 0, errors.New("Invalid character escape")
			}
			v, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			b.WriteByte(byte(v))
			i += 2
		case e >= '0' && e <= '9':
			j := i
			for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			v, err := strconv.ParseUint(s[i:j], 8, 16)
			if err != nil || v > 255 {
				return "", 0, errors.New("Invalid character escape")
			}
			b.WriteByte(byte(v))
			i = j - 1
		default:
			return "", 0, errors.New("Invalid character escape")
		}
	}
	return "", 0, errors.New("Premature program end")
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// operators are the math operators, as the values calculate receives.
const (
	operPlus = iota
	operMinus
	operStar
	operSlash
	operModulo
)

type parser struct {
	toks   []token
	pos    int
	prog   *Program
	cur    *program // the program statements go to
	params []param
	idx    int
}

// Parse parses a template.
func Parse(s string) (*Program, error) {
	if len(s) > maxProgramSize {
		return nil, fmt.Errorf("Program size exceeds maximum of %d bytes", maxProgramSize)
	}
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, prog: &Program{}}
	for p.peek().kind != tEOF {
		if err := p.expression(); err != nil {
			return nil, err
		}
		p.cur = nil
	}
	return p.prog, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}

func (p *parser) unexpected(expecting ...tokenKind) error {
	t := p.peek()
	names := make([]string, len(expecting))
	for i, k := range expecting {
		names[i] = tokenNames[k]
	}
	return fmt.Errorf("syntax error, unexpected %s, expecting %s", tokenNames[t.kind], strings.Join(names, " or "))
}

func (p *parser) expression() error {
	switch t := p.next(); t.kind {
	case tValue:
		p.literal(t.text)
	case tPerc:
		p.literal("%")
	case tOCBrace:
		if err := p.filterList(); err != nil {
			return err
		}
		if p.peek().kind != tCCBrace {
			return p.unexpected(tCCBrace, tPipe, tName)
		}
		p.next()
	default:
		p.pos--
		return p.unexpected(tValue, tOCBrace, tPerc)
	}
	return nil
}

func (p *parser) literal(s string) {
	p.pushArg(param{typ: paramString, str: s})
	p.pushFunction("literal")
	p.cur.onlyLiteral = true
}

func (p *parser) filterList() error {
	for {
		if err := p.filter(); err != nil {
			return err
		}
		if p.peek().kind != tPipe {
			return nil
		}
		p.next()
	}
}

func (p *parser) filter() error {
	if p.peek().kind != tName {
		return nil // an empty filter
	}
	name := p.next().text
	if p.peek().kind == tOBrace {
		p.next()
		if err := p.arguments(); err != nil {
			return err
		}
	}
	p.pushFunction(name)
	if op, ok := p.operator(); ok {
		p.pushArg(param{typ: paramInt, num: int64(op)})
		if p.peek().kind == tName {
			p.pushArg(param{typ: paramVariable, str: p.next().text})
		} else {
			n, err := p.number()
			if err != nil {
				return err
			}
			p.pushArg(param{typ: paramInt, num: n})
		}
		p.pushFunction("calculate")
	}
	return nil
}

func (p *parser) operator() (int, bool) {
	switch p.peek().kind {
	case tPlus:
		p.next()
		return operPlus, true
	case tMinus:
		p.next()
		return operMinus, true
	case tStar:
		p.next()
		return operStar, true
	case tSlash:
		p.next()
		return operSlash, true
	case tPerc:
		p.next()
		return operModulo, true
	}
	return 0, false
}

func (p *parser) number() (int64, error) {
	neg := false
	if p.peek().kind == tMinus {
		p.next()
		neg = true
	}
	if p.peek().kind != tNumber {
		return 0, p.unexpected(tNumber)
	}
	text := p.next().text
	if neg {
		text = "-" + text
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, errors.New("Not a number")
	}
	return n, nil
}

func (p *parser) arguments() error {
	for {
		if err := p.argument(); err != nil {
			return err
		}
		switch p.peek().kind {
		case tComma:
			p.next()
		case tCBrace:
			p.next()
			return nil
		default:
			return p.unexpected(tCBrace, tComma)
		}
	}
}

func (p *parser) argument() error {
	switch t := p.peek(); t.kind {
	case tValue:
		p.next()
		p.pushArg(param{typ: paramString, str: t.text})
	case tName:
		p.next()
		if p.peek().kind != tEq {
			p.pushArg(param{typ: paramVariable, str: t.text})
			return nil
		}
		p.next()
		key := strings.ToLower(t.text)
		switch v := p.peek(); v.kind {
		case tName:
			p.next()
			p.pushNamed(param{key: key, typ: paramVariable, str: v.text})
		case tValue:
			p.next()
			p.pushNamed(param{key: key, typ: paramString, str: v.text})
		default:
			n, err := p.number()
			if err != nil {
				return err
			}
			p.pushNamed(param{key: key, typ: paramInt, num: n})
		}
	case tNumber, tMinus:
		n, err := p.number()
		if err != nil {
			return err
		}
		p.pushArg(param{typ: paramInt, num: n})
	default:
		return p.unexpected(tValue, tName, tNumber)
	}
	return nil
}

func (p *parser) pushArg(par param) {
	par.idx = p.idx
	p.idx++
	p.params = append(p.params, par)
}

func (p *parser) pushNamed(par param) {
	par.idx = -1
	p.params = append(p.params, par)
}

// pushFunction ends a statement: positional arguments first in order, then
// named ones by key, as the reference orders them.
func (p *parser) pushFunction(name string) {
	if p.cur == nil {
		p.cur = &program{}
		p.prog.programs = append(p.prog.programs, p.cur)
	}
	params := p.params
	sort.SliceStable(params, func(i, j int) bool {
		a, b := params[i], params[j]
		if (a.idx >= 0) != (b.idx >= 0) {
			return a.idx >= 0
		}
		if a.idx >= 0 {
			return a.idx < b.idx
		}
		return a.key < b.key
	})
	p.cur.stmts = append(p.cur.stmts, statement{function: name, params: params})
	p.params, p.idx = nil, 0
}
