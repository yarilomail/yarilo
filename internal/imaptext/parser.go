package imaptext

import (
	"strconv"
	"strings"
	"time"
)

// arg is one parsed IMAP item: an nstring, an atom, or a list of them.
type arg struct {
	str     string
	present bool // false for NIL
	list    bool
	items   []arg
}

type parser struct {
	in string
	at int
}

func (p *parser) eof() bool { return p.at >= len(p.in) }

func (p *parser) space() bool {
	if p.at < len(p.in) && p.in[p.at] == ' ' {
		p.at++
		return true
	}
	return false
}

// arg reads one item. Depth is bounded by the caller: the envelope nests twice
// and a body structure is bounded where it recurses.
func (p *parser) arg() (arg, bool) {
	if p.eof() {
		return arg{}, false
	}
	switch p.in[p.at] {
	case '(':
		return p.parseList()
	case '"':
		return p.parseQuoted()
	case '{':
		return p.parseLiteral()
	default:
		return p.parseAtom()
	}
}

func (p *parser) parseList() (arg, bool) {
	p.at++ // '('
	out := arg{list: true, present: true}
	for {
		if p.eof() {
			return arg{}, false
		}
		if p.in[p.at] == ')' {
			p.at++
			return out, true
		}
		// Address structures follow one another with no separator: the
		// reference's writer emits "(..)(..)" inside one list.
		if len(out.items) > 0 && !p.space() && p.in[p.at] != '(' {
			return arg{}, false
		}
		item, ok := p.arg()
		if !ok {
			return arg{}, false
		}
		out.items = append(out.items, item)
	}
}

func (p *parser) parseQuoted() (arg, bool) {
	p.at++ // '"'
	var b strings.Builder
	for p.at < len(p.in) {
		c := p.in[p.at]
		switch c {
		case '"':
			p.at++
			return arg{str: b.String(), present: true}, true
		case '\\':
			if p.at+1 >= len(p.in) {
				return arg{}, false
			}
			p.at++
			b.WriteByte(p.in[p.at])
		case '\r', '\n':
			return arg{}, false
		default:
			b.WriteByte(c)
		}
		p.at++
	}
	return arg{}, false
}

func (p *parser) parseLiteral() (arg, bool) {
	end := strings.Index(p.in[p.at:], "}\r\n")
	if end < 0 {
		return arg{}, false
	}
	n, err := strconv.Atoi(p.in[p.at+1 : p.at+end])
	if err != nil || n < 0 {
		return arg{}, false
	}
	start := p.at + end + 3
	if start+n > len(p.in) {
		return arg{}, false
	}
	p.at = start + n
	return arg{str: p.in[start : start+n], present: true}, true
}

func (p *parser) parseAtom() (arg, bool) {
	start := p.at
	for p.at < len(p.in) && p.in[p.at] != ' ' && p.in[p.at] != ')' {
		p.at++
	}
	s := p.in[start:p.at]
	if s == "" {
		return arg{}, false
	}
	if strings.EqualFold(s, "NIL") {
		return arg{}, true
	}
	return arg{str: s, present: true}, true
}

// messageDateLayouts are the Date header forms a cached envelope can carry;
// the first is what we write.
var messageDateLayouts = []string{
	"Mon, 02 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"02 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 -0700",
	"Mon, 02 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 2006 15:04:05 MST",
}

func parseMessageDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range messageDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
