package msgcache

import (
	"encoding/binary"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The reference's field table (index-mail.c:29-68). The names and the types
// are theirs: a cache is a container both implementations read (#1714).
const (
	fieldDateSent          = "date.sent"
	fieldDateReceived      = "date.received"
	fieldDateSave          = "date.save"
	fieldSizeVirtual       = "size.virtual"
	fieldSizePhysical      = "size.physical"
	fieldIMAPBody          = "imap.body"
	fieldIMAPBodyStructure = "imap.bodystructure"
	fieldIMAPEnvelope      = "imap.envelope"
	fieldPOP3UIDL          = "pop3.uidl"
	fieldPOP3Order         = "pop3.order"
	fieldGUID              = "guid"
	fieldHdrReferences     = "hdr.references"
)

// The reference's list, in its order (message-part-data.c:14-18): their cache
// holds these instead of a built envelope, so ours holds them too (#1714).
var envelopeHeaders = []string{
	"Date", "Subject", "From", "Sender", "Reply-To",
	"To", "Cc", "Bcc", "In-Reply-To", "Message-ID",
}

// headerField is the cache field name for one header.
func headerField(name string) string { return "hdr." + name }

// Case-insensitive: a table written elsewhere spells hdr.MESSAGE-ID where ours
// says hdr.Message-ID (mail-cache.c:575-576).
func (fc *Handle) fieldID(name string) uint32 {
	return fc.ids[strings.ToLower(name)]
}

// Sizes are the reference's struct members: uoff_t is 8, a date 4, and
// date.sent carries the header's timezone too (index-mail.h:63-66).
var referenceFields = withEnvelopeHeaders([]mailindex.CacheField{
	{Name: fieldSizePhysical, Type: mailindex.CacheFieldFixedSize, Size: 8, Decision: mailindex.CacheDecisionYes},
	{Name: fieldSizeVirtual, Type: mailindex.CacheFieldFixedSize, Size: 8, Decision: mailindex.CacheDecisionYes},
	{Name: fieldDateReceived, Type: mailindex.CacheFieldFixedSize, Size: 4, Decision: mailindex.CacheDecisionYes},
	{Name: fieldDateSent, Type: mailindex.CacheFieldFixedSize, Size: 8, Decision: mailindex.CacheDecisionYes},
	{Name: fieldDateSave, Type: mailindex.CacheFieldFixedSize, Size: 4, Decision: mailindex.CacheDecisionYes},
	{Name: fieldIMAPEnvelope, Type: mailindex.CacheFieldString, Decision: mailindex.CacheDecisionYes},
	{Name: fieldIMAPBodyStructure, Type: mailindex.CacheFieldString, Decision: mailindex.CacheDecisionYes},
	{Name: fieldIMAPBody, Type: mailindex.CacheFieldString, Decision: mailindex.CacheDecisionYes},
	{Name: fieldGUID, Type: mailindex.CacheFieldString, Decision: mailindex.CacheDecisionYes},
	{Name: fieldPOP3UIDL, Type: mailindex.CacheFieldString, Decision: mailindex.CacheDecisionYes},
	{Name: fieldPOP3Order, Type: mailindex.CacheFieldFixedSize, Size: 4, Decision: mailindex.CacheDecisionYes},
	{Name: fieldHdrReferences, Type: mailindex.CacheFieldHeader, Decision: mailindex.CacheDecisionYes},
})

// withEnvelopeHeaders appends the ten header fields to the fixed table, so the
// list stays one spelling rather than ten more literals.
func withEnvelopeHeaders(fields []mailindex.CacheField) []mailindex.CacheField {
	for _, h := range envelopeHeaders {
		fields = append(fields, mailindex.CacheField{
			Name: headerField(h), Type: mailindex.CacheFieldHeader, Decision: mailindex.CacheDecisionYes,
		})
	}
	return fields
}

func encodeU32(v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return b[:]
}

func decodeU32(b []byte) (uint32, bool) {
	if len(b) < 4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b), true
}

func encodeU64(v uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return b[:]
}

func decodeU64(b []byte) (uint64, bool) {
	if len(b) < 8 {
		return 0, false
	}
	return binary.LittleEndian.Uint64(b), true
}

// encodeSentDate is the reference's mail_sent_date: the instant, then the
// offset the header carried in minutes.
func encodeSentDate(t time.Time) []byte {
	_, offset := t.Zone()
	b := make([]byte, 0, 8)
	b = append(b, encodeU32(uint32(t.Unix()))...)
	return append(b, encodeU32(uint32(int32(offset/60)))...)
}

func decodeSentDate(b []byte) (time.Time, bool) {
	if len(b) < 8 {
		return time.Time{}, false
	}
	secs := binary.LittleEndian.Uint32(b[:4])
	if secs == 0 {
		return time.Time{}, false
	}
	tz := int32(binary.LittleEndian.Uint32(b[4:8]))
	return time.Unix(int64(secs), 0).In(time.FixedZone("", int(tz)*60)), true
}

// The reference's header payload: line numbers, a zero, then the header text
// (index-mail-headers.c:99-127).
func encodeHeaderField(lineNum uint32, text string) []byte {
	b := make([]byte, 0, 8+len(text))
	b = append(b, encodeU32(lineNum)...)
	b = append(b, encodeU32(0)...)
	return append(b, text...)
}

// decodeHeaderField returns the header text a payload carries, skipping the
// line numbers. An empty payload means the message has no such header.
func decodeHeaderField(b []byte) (string, bool) {
	for len(b) >= 4 {
		n := binary.LittleEndian.Uint32(b[:4])
		b = b[4:]
		if n == 0 {
			return string(b), true
		}
	}
	if len(b) == 0 {
		return "", true
	}
	return "", false
}

// headerValue strips the field name from a cached header line, which is stored
// whole so a reader can serve BODY[HEADER.FIELDS] from it.
func headerValue(line string) string {
	if i := strings.IndexByte(line, ':'); i >= 0 {
		line = line[i+1:]
	}
	return strings.TrimSpace(unfoldHeader(line))
}

func unfoldHeader(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// The whole line, as the reference caches a header. An empty list is an empty
// payload: "none" is an answer, not a permanent miss.
func encodeReferencesHeader(refs []string) []byte {
	if len(refs) == 0 {
		return nil
	}
	return encodeHeaderField(1, "References: "+strings.Join(refs, " ")+"\r\n")
}

// referencesFromHeader is the inverse. The bool is "this was cached", so an
// empty answer is not read as a miss.
func referencesFromHeader(b []byte) ([]string, bool) {
	if len(b) == 0 {
		return nil, true
	}
	line, ok := decodeHeaderField(b)
	if !ok {
		return nil, false
	}
	value := headerValue(line)
	if value == "" {
		return nil, true
	}
	return strings.Fields(value), true
}

// Sizes returns the physical and virtual sizes the cache holds. The reference
// reads them here first too, before it opens anything (index-mail.c).
func (fc *Handle) Sizes(m *mailbox.MessageMeta) (size, vsize uint32, ok bool) {
	if fc == nil {
		return 0, 0, false
	}
	vals := fc.read(m)
	phys, pok := decodeU64(vals[fc.fieldID(fieldSizePhysical)])
	virt, vok := decodeU64(vals[fc.fieldID(fieldSizeVirtual)])
	if !pok || !vok {
		return 0, 0, false
	}
	return uint32(phys), uint32(virt), true
}

// StoreSizes caches both sizes. A zero is not stored: it is the value that
// means "not measured", and caching it would make the miss permanent.
func (fc *Handle) StoreSizes(m *mailbox.MessageMeta, size, vsize uint32) {
	if fc == nil || size == 0 || vsize == 0 {
		return
	}
	fc.storeField(m, fc.fieldID(fieldSizePhysical), encodeU64(uint64(size)))
	fc.storeField(m, fc.fieldID(fieldSizeVirtual), encodeU64(uint64(vsize)))
}

// ReceivedDate returns the cached internal date.
func (fc *Handle) ReceivedDate(m *mailbox.MessageMeta) (time.Time, bool) {
	if fc == nil {
		return time.Time{}, false
	}
	secs, ok := decodeU32(fc.read(m)[fc.fieldID(fieldDateReceived)])
	if !ok || secs == 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(secs), 0).UTC(), true
}

// SentDate returns the cached Date header with the offset it carried.
func (fc *Handle) SentDate(m *mailbox.MessageMeta) (time.Time, bool) {
	if fc == nil {
		return time.Time{}, false
	}
	return decodeSentDate(fc.read(m)[fc.fieldID(fieldDateSent)])
}

// GUID returns the cached message identifier, as the reference stores it: the
// hex string a client is shown.
func (fc *Handle) GUID(m *mailbox.MessageMeta) (string, bool) {
	if fc == nil {
		return "", false
	}
	data, ok := fc.read(m)[fc.fieldID(fieldGUID)]
	if !ok || len(data) == 0 {
		return "", false
	}
	return string(data), true
}

// POP3UIDL returns the cached POP3 identifier for a message.
func (fc *Handle) POP3UIDL(m *mailbox.MessageMeta) (string, bool) {
	if fc == nil {
		return "", false
	}
	data, ok := fc.read(m)[fc.fieldID(fieldPOP3UIDL)]
	if !ok || len(data) == 0 {
		return "", false
	}
	return string(data), true
}

// StorePOP3UIDL caches the POP3 identifier and the order it was listed in,
// which is where the reference keeps both.
func (fc *Handle) StorePOP3UIDL(m *mailbox.MessageMeta, uidl string, order uint32) {
	if fc == nil || uidl == "" {
		return
	}
	fc.storeField(m, fc.fieldID(fieldPOP3UIDL), []byte(uidl))
	if order != 0 {
		fc.storeField(m, fc.fieldID(fieldPOP3Order), encodeU32(order))
	}
}

// StoreRecordFields caches what the record already knows -- sizes, dates,
// identifier -- while the message is open anyway for its envelope.
func (fc *Handle) StoreRecordFields(m *mailbox.MessageMeta) {
	if fc == nil || m == nil {
		return
	}
	fc.StoreSizes(m, m.Size, m.VSize)
	if !m.InternalDate.IsZero() {
		fc.storeField(m, fc.fieldID(fieldDateReceived), encodeU32(uint32(m.InternalDate.Unix())))
	}
	if m.SaveDate != 0 {
		fc.storeField(m, fc.fieldID(fieldDateSave), encodeU32(m.SaveDate))
	}
	if m.GUID != ([16]byte{}) {
		fc.storeField(m, fc.fieldID(fieldGUID), []byte(mailbox.FormatObjectID(m.GUID)))
	}
}

// StoreSentDate caches the Date header as parsed, with its offset.
func (fc *Handle) StoreSentDate(m *mailbox.MessageMeta, t time.Time) {
	if fc == nil || t.IsZero() {
		return
	}
	fc.storeField(m, fc.fieldID(fieldDateSent), encodeSentDate(t))
}
