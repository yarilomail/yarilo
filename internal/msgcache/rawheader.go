package msgcache

import (
	"github.com/emersion/go-message/textproto"

	"github.com/yarilomail/yarilo/internal/imaptext"
)

// RawHeader gives imaptext the header values as the message carried them.
// go-message decodes nothing on read, which is what the envelope needs: the
// reference caches the header's own bytes (#1714).
type RawHeader struct {
	H textproto.Header
}

func (r RawHeader) RawValues(name string) []string {
	var out []string
	fields := r.H.FieldsByKey(name)
	for fields.Next() {
		out = append(out, fields.Value())
	}
	return out
}

// EnvelopeTextOf builds the envelope text for a message's header.
func EnvelopeTextOf(h textproto.Header) string {
	return imaptext.EnvelopeFromHeader(RawHeader{H: h})
}
