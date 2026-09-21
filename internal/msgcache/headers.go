package msgcache

import (
	"github.com/emersion/go-message/textproto"

	"github.com/yarilomail/yarilo/internal/imaptext"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// cachedHeaders answers imaptext out of a message's cache record: the header
// lines the reference stores, whole, under hdr.<name>.
type cachedHeaders struct {
	fc   *Handle
	vals map[uint32][]byte
}

func (c cachedHeaders) RawValues(name string) []string {
	id := c.fc.fieldID(headerField(name))
	data, ok := c.vals[id]
	if !ok || len(data) == 0 {
		return nil
	}
	line, ok := decodeHeaderField(data)
	if !ok {
		return nil
	}
	if value := headerValue(line); value != "" {
		return []string{value}
	}
	return nil
}

// envelopeFromCachedHeaders builds the envelope from the headers a record
// holds, which is what a cache from the reference carries instead of a built
// one (index-mail-headers.c:515-560).
func (fc *Handle) envelopeFromCachedHeaders(vals map[uint32][]byte) (string, bool) {
	h := cachedHeaders{fc: fc, vals: vals}
	var any bool
	for _, name := range envelopeHeaders {
		if len(h.RawValues(name)) > 0 {
			any = true
			break
		}
	}
	if !any {
		return "", false
	}
	return imaptext.EnvelopeFromHeader(h), true
}

// storeEnvelopeHeaders caches the ten headers the envelope is built from, as
// the reference caches them: one field per header, the line kept whole so a
// reader can serve BODY[HEADER.FIELDS] from it.
func (fc *Handle) storeEnvelopeHeaders(m *mailbox.MessageMeta, h textproto.Header) {
	for _, name := range envelopeHeaders {
		fields := h.FieldsByKey(name)
		if !fields.Next() {
			continue
		}
		line := name + ": " + fields.Value() + "\r\n"
		fc.storeField(m, fc.fieldID(headerField(name)), encodeHeaderField(1, line))
	}
}
