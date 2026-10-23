package protocol

import "strings"

// TabEscape makes a value safe for one field of an auth line: \x01 is the
// escape, followed by 1, t, r or n; a NUL is dropped.
func TabEscape(s string) string {
	if !strings.ContainsAny(s, "\x00\x01\t\r\n") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 0:
		case 1:
			b.WriteString("\x011")
		case '\t':
			b.WriteString("\x01t")
		case '\r':
			b.WriteString("\x01r")
		case '\n':
			b.WriteString("\x01n")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// TabUnescape reverses TabEscape; an unknown escape keeps its byte.
func TabUnescape(s string) string {
	if !strings.Contains(s, "\x01") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 1 || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case '1':
			b.WriteByte(1)
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
