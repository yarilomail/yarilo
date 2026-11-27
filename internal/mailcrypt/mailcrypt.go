// Package mailcrypt tells the three kinds of stored message body apart.
package mailcrypt

import (
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/agestream"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

// Kind is what a stored body is, judged by its first bytes.
type Kind int

const (
	Plain     Kind = iota
	Reference      // the reference server's encrypted stream (refstream)
	Age            // ours (agestream)
)

// PrefixLen is how many leading bytes Classify needs.
const PrefixLen = 22

// Classify reads the signature at the start of a body. A body yarilo did not
// encrypt that classifies as Reference or Age is refused on save.
func Classify(prefix []byte) Kind {
	switch {
	case agestream.IsEncrypted(prefix):
		return Age
	case refstream.IsEncrypted(prefix):
		return Reference
	}
	return Plain
}
