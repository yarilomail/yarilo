package imaptext

import "strings"

// HeaderLookup gives a header's raw values, in the order the message carried
// them and with nothing decoded.
type HeaderLookup interface {
	RawValues(name string) []string
}

// EnvelopeFromHeader builds the ENVELOPE text from the raw header as the
// reference fills it: nothing decoded, groups kept (message-part-data.c:200-253).
func EnvelopeFromHeader(h HeaderLookup) string {
	var b strings.Builder
	date := joinHeader(h, "Date")
	AppendNString(&b, date, date != "")
	b.WriteByte(' ')
	if subject := joinHeader(h, "Subject"); subject == "" {
		b.WriteString("NIL")
	} else {
		AppendStringForHumans(&b, subject)
	}

	from := parseAddressList(joinHeader(h, "From"))
	sender := parseAddressList(joinHeader(h, "Sender"))
	replyTo := parseAddressList(joinHeader(h, "Reply-To"))
	if len(sender) == 0 {
		sender = from
	}
	if len(replyTo) == 0 {
		replyTo = from
	}
	for _, list := range [][]addrItem{
		from, sender, replyTo,
		parseAddressList(joinHeader(h, "To")),
		parseAddressList(joinHeader(h, "Cc")),
		parseAddressList(joinHeader(h, "Bcc")),
	} {
		b.WriteByte(' ')
		writeParsedAddresses(&b, list)
	}

	b.WriteByte(' ')
	inReplyTo := joinHeader(h, "In-Reply-To")
	AppendNString(&b, inReplyTo, inReplyTo != "")
	b.WriteByte(' ')
	messageID := joinHeader(h, "Message-Id")
	AppendNString(&b, messageID, messageID != "")
	return b.String()
}

// Repeated headers merge comma-separated: one From must not be shown while
// another was validated (message-part-data.c:238-248).
func joinHeader(h HeaderLookup, name string) string {
	values := h.RawValues(name)
	for i, v := range values {
		values[i] = strings.TrimSpace(unfold(v))
	}
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	}
	return strings.Join(values, ", ")
}

// unfold turns a folded header value into one line; a run of whitespace is one
// space, which is what every writer below assumes.
func unfold(v string) string {
	var b strings.Builder
	lastSpace := false
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\r', '\n', '\t', ' ':
			if !lastSpace {
				b.WriteByte(' ')
			}
			lastSpace = true
		default:
			b.WriteByte(c)
			lastSpace = false
		}
	}
	return b.String()
}

func writeParsedAddresses(b *strings.Builder, addrs []addrItem) {
	if len(addrs) == 0 {
		b.WriteString("NIL")
		return
	}
	b.WriteByte('(')
	for _, a := range addrs {
		b.WriteByte('(')
		switch {
		case a.group:
			// The opener names the group in the mailbox slot, the closer
			// carries nothing: RFC 3501 §7.4.2.
			b.WriteString("NIL NIL ")
			AppendNString(b, a.mailbox, a.mailbox != "")
			b.WriteString(" NIL")
		default:
			if a.name == "" {
				b.WriteString("NIL")
			} else {
				AppendStringForHumans(b, a.name)
			}
			b.WriteString(" NIL ")
			AppendNString(b, a.mailbox, a.mailbox != "")
			b.WriteByte(' ')
			AppendNString(b, a.host, a.host != "")
		}
		b.WriteByte(')')
	}
	b.WriteByte(')')
}
