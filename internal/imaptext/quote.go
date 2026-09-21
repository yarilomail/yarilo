// Package imaptext writes and reads the IMAP text the reference keeps in its
// index cache: the ENVELOPE and BODYSTRUCTURE strings, byte for byte.
//
// Byte-for-byte is the point. A cache written here is read by the reference and
// one written by the reference is read here, which is what makes the cache a
// shared container rather than ours in their file (#1714).
package imaptext

import (
	"fmt"
	"strings"
)

// quotedMaxEscapeChars is the reference's cut-off: past this many escapes a
// quoted string becomes a literal (imap-quote.c:16).
const quotedMaxEscapeChars = 4

// AppendNString writes s as the reference's imap_append_nstring does:
// NIL for absent, an atom-safe run bare, otherwise quoted or literal
// (imap-quote.c:45-91).
func AppendNString(b *strings.Builder, s string, present bool) {
	if !present {
		b.WriteString("NIL")
		return
	}
	appendString(b, s)
}

// AppendAString writes an astring: bare when every byte is astring-safe and the
// run is neither empty nor the word NIL (imap-quote.c:24-42).
func AppendAString(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		if !isAStringChar(s[i]) {
			appendString(b, s)
			return
		}
	}
	if s == "" || strings.EqualFold(s, "NIL") {
		appendString(b, s)
		return
	}
	b.WriteString(s)
}

// appendString is imap_append_string: quoted where the reference would quote,
// a literal at the first byte that forbids quoting.
func appendString(b *strings.Builder, s string) {
	escapes := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			escapes++
			if escapes <= quotedMaxEscapeChars {
				continue
			}
			appendLiteral(b, s)
			return
		case '\r', '\n':
			appendLiteral(b, s)
			return
		default:
			if c >= 0x80 {
				appendLiteral(b, s)
				return
			}
		}
	}
	appendQuoted(b, s)
}

func appendQuoted(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\r', '\n': // not allowed inside a quoted string
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

func appendLiteral(b *strings.Builder, s string) {
	fmt.Fprintf(b, "{%d}\r\n", len(s))
	b.WriteString(s)
}

// AppendStringForHumans is the subject and display-name form: runs of
// whitespace collapse, NUL becomes 0x80, and the result is quoted when nothing
// had to change (imap-quote.c:152-240).
func AppendStringForHumans(b *strings.Builder, s string) {
	var removed int
	lastLWSP, wsPrefix, modify := true, true, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case 0:
			lastLWSP, modify = false, true
		case '\r', '\n', '\t', ' ':
			if c != ' ' {
				modify = true
			}
			if lastLWSP {
				modify = true
				removed++
			}
			lastLWSP = true
		case '"', '\\':
			modify, lastLWSP = true, false
		default:
			if c >= 0x80 {
				modify = true
			}
			lastLWSP = false
		}
		if !lastLWSP {
			wsPrefix = false
		}
	}
	if lastLWSP && len(s) > 0 && !wsPrefix {
		modify = true
		removed++
	}
	if !modify {
		b.WriteByte('"')
		b.WriteString(s)
		b.WriteByte('"')
		return
	}
	if len(s) == removed {
		b.WriteString(`""`)
		return
	}

	var out strings.Builder
	lastLWSP, wsPrefix = true, true
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case 0:
			out.WriteByte(0x80)
			lastLWSP = false
		case '\r', '\n', '\t', ' ':
			if !lastLWSP {
				out.WriteByte(' ')
			}
			lastLWSP = true
		default:
			out.WriteByte(c)
			lastLWSP = false
		}
		if !lastLWSP {
			wsPrefix = false
		}
	}
	body := out.String()
	if lastLWSP && len(s) > 0 && !wsPrefix {
		body = body[:len(body)-1]
	}
	fmt.Fprintf(b, "{%d}\r\n", len(body))
	b.WriteString(body)
}

// isAStringChar is the reference's ASTRING-CHAR: an atom char, plus "]"
// (imap-arg.h:14-30). 8-bit is atom-special there, so it is here.
func isAStringChar(c byte) bool {
	switch c {
	case '(', ')', '{', '%', '*', '"', '\\':
		return false
	}
	return c > 0x20 && c < 0x7f
}
