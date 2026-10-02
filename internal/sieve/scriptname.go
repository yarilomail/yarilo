package sieve

import (
	"errors"
	"unicode/utf8"
)

// maxScriptNameLen is the longest script name, in characters.
const maxScriptNameLen = 256

// ErrInvalidScriptName is returned for a name ValidScriptName refuses.
var ErrInvalidScriptName = errors.New("sieve: invalid script name")

// ValidScriptName reports whether name may name a stored script: valid UTF-8,
// 1 to 256 characters, no control or separator characters, no path element.
func ValidScriptName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > maxScriptNameLen*4 ||
		!utf8.ValidString(name) || utf8.RuneCountInString(name) > maxScriptNameLen {
		return false
	}
	for _, r := range name {
		switch {
		case r <= 0x1f, r == 0x7f, r >= 0x80 && r <= 0x9f, r == 0xff,
			r == '/', r == '\\', r == 0x2028, r == 0x2029:
			return false
		}
	}
	return true
}
