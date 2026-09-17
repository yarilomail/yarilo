// Package proxy speaks the dict protocol to yarilo-dict, so a session holds a
// dict by name while the engines live in one process (#1733).
package proxy

import (
	"fmt"
	"strconv"
	"strings"
)

// Protocol version on the wire. Minor 2 is what carries multi-value replies.
const (
	VersionMajor = 3
	VersionMinor = 2
)

// Opcodes, as INTERNALS.md §32 records them.
const (
	OpHello      = 'H'
	OpLookup     = 'L'
	OpIterate    = 'I'
	OpBegin      = 'B'
	OpCommit     = 'C'
	OpRollback   = 'R'
	OpSet        = 'S'
	OpUnset      = 'U'
	OpAtomicInc  = 'A'
	ReplyOK      = 'O'
	ReplyMulti   = 'M'
	ReplyNoMatch = 'N'
	ReplyFail    = 'F'
	ReplyUncert  = 'W'
)

// MaxLine bounds one line; a value larger than this is refused rather than
// buffered, because a dict value is a mailbox's data, not a stream.
const MaxLine = 64 << 10

// EscapeValue keeps a value on one line: the protocol is line-delimited, so a
// tab or newline inside a value would be read as structure.
func EscapeValue(v []byte) string {
	var b strings.Builder
	for _, c := range v {
		switch c {
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// UnescapeValue reverses EscapeValue.
func UnescapeValue(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out = append(out, s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			out = append(out, '\t')
		case 'n':
			out = append(out, '\n')
		default:
			out = append(out, s[i])
		}
	}
	return out
}

// helloLine is the greeting: the dict is named, never described. The URI lives
// in the server's config, as the reference keeps it.
func helloLine(valueType int, user, name string) string {
	return fmt.Sprintf("%c%d\t%d\t%d\t%s\t%s\n", OpHello, VersionMajor, VersionMinor, valueType, user, name)
}

// ParseHello reads the greeting on the server side.
func ParseHello(line string) (valueType int, user, name string, err error) {
	f := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
	if len(f) != 5 {
		return 0, "", "", fmt.Errorf("dict/proxy: malformed hello")
	}
	major, merr := strconv.Atoi(strings.TrimPrefix(f[0], string(OpHello)))
	if merr != nil || major != VersionMajor {
		return 0, "", "", fmt.Errorf("dict/proxy: unsupported major version %q", f[0])
	}
	vt, verr := strconv.Atoi(f[2])
	if verr != nil {
		return 0, "", "", fmt.Errorf("dict/proxy: malformed value type")
	}
	return vt, f[3], f[4], nil
}
