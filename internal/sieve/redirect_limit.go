package sieve

import (
	"bytes"
	"errors"
	"strings"

	gosieve "github.com/foxcpp/go-sieve"
	"github.com/foxcpp/go-sieve/lexer"
	"github.com/foxcpp/go-sieve/parser"
)

// ErrRedirectProhibited refuses a script with redirect while
// sieve_max_redirects is 0.
var ErrRedirectProhibited = errors.New("local policy prohibits the use of a redirect action")

// CheckRedirects refuses a script that uses redirect when maxRedirects is 0.
// The script is one gosieve.Load has already accepted.
func CheckRedirects(src []byte, maxRedirects int) error {
	if maxRedirects != 0 {
		return nil
	}
	opts := gosieve.DefaultOptions()
	toks, err := lexer.Lex(bytes.NewReader(src), &opts.Lexer)
	if err != nil {
		return nil
	}
	cmds, err := parser.Parse(lexer.NewStream(toks), &opts.Parser)
	if err != nil {
		return nil
	}
	if usesRedirect(cmds) {
		return ErrRedirectProhibited
	}
	return nil
}

func usesRedirect(cmds []parser.Cmd) bool {
	for _, c := range cmds {
		if strings.EqualFold(c.Id, "redirect") || usesRedirect(c.Block) {
			return true
		}
	}
	return false
}

// sameRedirectTarget compares two redirect targets as addresses: the local
// part exactly, the domain without case.
func sameRedirectTarget(a, b string) bool {
	ia, ib := strings.LastIndexByte(a, '@'), strings.LastIndexByte(b, '@')
	if ia < 0 || ib < 0 {
		return a == b
	}
	return a[:ia] == b[:ib] && strings.EqualFold(a[ia+1:], b[ib+1:])
}

// addRedirect merges a repeat of a target already redirected to: the message
// is sent to it once.
func (r *FilterResult) addRedirect(rd Redirect) {
	for i, have := range r.Redirects {
		if sameRedirectTarget(have.Address, rd.Address) {
			r.Redirects[i].Copy = have.Copy && rd.Copy
			return
		}
	}
	r.Redirects = append(r.Redirects, rd)
}
