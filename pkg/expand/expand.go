package expand

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Lookup answers a variable: its value, or an error when it cannot.
type Lookup func(name string) (string, error)

// Params is what a template is expanded against.
type Params struct {
	// Table holds plain variables; a missing name is an unknown variable.
	Table map[string]string
	// Funcs computes variables, tried before Table.
	Funcs map[string]Lookup
	// Providers answers prefix:name, beside the built-in ones.
	Providers map[string]Lookup
	// Escape is applied to every expanded value that is not a literal and not
	// marked safe; the escape filter applies it in a chain.
	Escape func(string) (string, error)
	// EscapeLiterals escapes the template's literal text too.
	EscapeLiterals bool
}

// ErrUnknown is the error of a variable no table holds.
var ErrUnknown = errors.New("unknown variable")

type state struct {
	params  *Params
	result  strings.Builder
	delayed string
	hasErr  bool

	transfer []byte
	set      bool
	binary   bool
	safe     bool
}

// Expand parses and runs a template.
func Expand(s string, p *Params) (string, error) {
	prog, err := Parse(s)
	if err != nil {
		return "", err
	}
	return prog.Execute(p)
}

// Execute runs the template.
func (prog *Program) Execute(p *Params) (string, error) {
	if p == nil {
		p = &Params{}
	}
	st := &state{params: p}
	for _, pr := range prog.programs {
		if err := st.run(pr); err != nil {
			return "", err
		}
	}
	if st.hasErr {
		return "", errors.New(st.delayed)
	}
	return st.result.String(), nil
}

func (st *state) run(pr *program) error {
	if len(pr.stmts) == 0 {
		return nil
	}
	for i := range pr.stmts {
		if err := st.statement(&pr.stmts[i], i == 0, i+1 < len(pr.stmts)); err != nil {
			return err
		}
	}
	if st.binary {
		st.setString(hex.EncodeToString(st.transfer))
	}
	if !st.set {
		st.unset()
		return errors.New(st.delayed)
	}
	out := string(st.transfer)
	if st.params.Escape != nil && !st.safe && (!pr.onlyLiteral || st.params.EscapeLiterals) {
		escaped, err := st.params.Escape(out)
		if err != nil {
			return err
		}
		out = escaped
	}
	st.result.WriteString(out)
	st.unset()
	return nil
}

// statement runs one step. A failing step is remembered rather than
// returned, so a later default() can still recover from it.
func (st *state) statement(s *statement, first, more bool) error {
	var delayed string
	hasDelayed := false
	if first {
		v, err := st.lookup(s.function)
		if err == nil {
			st.setString(v)
			return nil
		}
		st.unset()
		delayed, hasDelayed = err.Error(), true
	}
	if fn, ok := filters[s.function]; ok {
		err := fn(&call{st: st, stmt: s, more: more})
		hasDelayed, delayed = false, ""
		if err != nil {
			st.unset()
			if st.hasErr {
				return nil // the earlier error is the cause; keep it
			}
			delayed, hasDelayed = s.function+": "+err.Error(), true
		}
	} else if !first {
		return fmt.Errorf("No such function '%s'", s.function)
	}
	st.delayed, st.hasErr = delayed, hasDelayed
	return nil
}

// lookup answers a variable or prefix:name.
func (st *state) lookup(name string) (string, error) {
	if prefix, key, ok := strings.Cut(name, ":"); ok {
		return st.provider(prefix, key)
	}
	if fn, ok := st.params.Funcs[name]; ok {
		return fn(name)
	}
	if v, ok := st.params.Table[name]; ok {
		return v, nil
	}
	return "", fmt.Errorf("Unknown variable '%s'", name)
}

func (st *state) provider(prefix, key string) (string, error) {
	fn, ok := builtinProviders[prefix]
	if !ok {
		fn, ok = st.params.Providers[prefix]
	}
	if !ok {
		return "", fmt.Errorf("Unsupported prefix '%s'", prefix)
	}
	v, err := fn(key)
	if err != nil {
		return "", fmt.Errorf("%s: %w", prefix, err)
	}
	return v, nil
}

func (st *state) setData(b []byte) {
	st.transfer = append(st.transfer[:0], b...)
	st.set = true
}

func (st *state) setBinary(b []byte) {
	st.setData(b)
	st.binary = true
}

func (st *state) setString(s string) {
	st.setData([]byte(s))
	st.binary = false
}

func (st *state) unset() {
	st.transfer = st.transfer[:0]
	st.safe = false
	st.set = false
}

// call is one filter invocation.
type call struct {
	st   *state
	stmt *statement
	more bool // a statement follows this one
}

func (c *call) params() []param { return c.stmt.params }

func errUnsupportedKey(key string) error { return fmt.Errorf("Unsupported key '%s'", key) }

var errTooManyPositional = errors.New("Too many positional parameters")

func (c *call) noParams() error {
	if len(c.stmt.params) == 0 {
		return nil
	}
	if k := c.stmt.params[0].key; k != "" {
		return errUnsupportedKey(k)
	}
	return errTooManyPositional
}

func (c *call) needParams() error {
	if len(c.stmt.params) == 0 {
		return errors.New("Missing parameters")
	}
	return nil
}

func (c *call) needTransfer(action string) error {
	if !c.st.set {
		return fmt.Errorf("No value to %s", action)
	}
	return nil
}

// number is a parameter as a number, looked up when it names a variable.
func (c *call) number(p *param) (int64, error) {
	if p == nil {
		return 0, errors.New("Missing parameter")
	}
	switch p.typ {
	case paramVariable:
		v, err := c.st.lookup(p.str)
		if err != nil {
			return 0, err
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("'%s' (in %s) is not a number", v, p.str)
		}
		return n, nil
	case paramInt:
		return p.num, nil
	}
	return 0, fmt.Errorf("'%s' is not a number", p.str)
}

func (c *call) boolean(p *param) (bool, error) {
	n, err := c.number(p)
	if err != nil {
		return false, err
	}
	switch n {
	case 0:
		return false, nil
	case 1:
		return true, nil
	}
	return false, fmt.Errorf("'%d' is not 0 or 1", n)
}

func (c *call) str(p *param) (string, error) {
	if p == nil {
		return "", errors.New("Missing parameter")
	}
	switch p.typ {
	case paramVariable:
		return c.st.lookup(p.str)
	case paramInt:
		return "", fmt.Errorf("%d is not a string", p.num)
	}
	return p.str, nil
}

func (c *call) any(p *param) (string, error) {
	if p == nil {
		return "", errors.New("Missing parameter")
	}
	switch p.typ {
	case paramVariable:
		return c.st.lookup(p.str)
	case paramInt:
		return strconv.FormatInt(p.num, 10), nil
	}
	return p.str, nil
}

// fromTransfer is the input as a parameter, for if and switch.
func (c *call) fromTransfer(number bool) (*param, bool) {
	if !c.st.set {
		return nil, false
	}
	if number {
		n, err := strconv.ParseInt(string(c.st.transfer), 10, 64)
		if err != nil {
			return nil, false
		}
		return &param{idx: -1, typ: paramInt, num: n}, true
	}
	return &param{idx: -1, typ: paramString, str: string(c.st.transfer)}, true
}
