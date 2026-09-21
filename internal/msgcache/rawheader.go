package msgcache

import (
	"github.com/emersion/go-message/textproto"

	"github.com/yarilomail/yarilo/internal/imaptext"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// RawHeader gives imaptext the header values as the message carried them: the
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

// StoreFromHeader caches what one parse of a message's header yields: the
// envelope, the ten headers it was built from and the References. A cache of
// ours is then what a cache of theirs is after one FETCH ENVELOPE (#1714).
func (fc *Handle) StoreFromHeader(m *mailbox.MessageMeta, h textproto.Header, text string) {
	if fc == nil {
		return
	}
	fc.StoreEnvelopeText(m, text)
	fc.storeEnvelopeHeaders(m, h)
	fc.StoreRecordFields(m)
}
