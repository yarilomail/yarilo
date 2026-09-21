package msgcache

import (
	"encoding/binary"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
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

// referenceFields is what a folder's cache carries. Sizes match the reference's
// struct members: uoff_t is 8 bytes, a date is 4, and date.sent adds the
// timezone the header carried (index-mail.h:63-66).
var referenceFields = []mailindex.CacheField{
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

// encodeHeaderField is the reference's header payload: the line numbers the
// header occupied, a zero terminator, then the header text itself
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

// encodeReferencesHeader writes the References the way the reference caches a
// header: the whole line, so a reader can serve BODY[HEADER.FIELDS] from it.
// An empty list is an empty payload, which is how "this message has none" is
// cached rather than left a permanent miss.
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
