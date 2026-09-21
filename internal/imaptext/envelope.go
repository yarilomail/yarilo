package imaptext

import (
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

// WriteEnvelope returns the ten ENVELOPE items the reference caches, space
// separated and without the enclosing parentheses (imap-envelope.c:47-87).
func WriteEnvelope(env *imaplib.Envelope) string {
	if env == nil {
		return "NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL"
	}
	var b strings.Builder
	AppendNString(&b, envelopeDate(env), !env.Date.IsZero())
	b.WriteByte(' ')
	if env.Subject == "" {
		b.WriteString("NIL")
	} else {
		AppendStringForHumans(&b, env.Subject)
	}
	// sender and reply-to fall back to from, as the reference's NVL does: a
	// reader of ours must see the same lists a reader of theirs sees.
	sender, replyTo := env.Sender, env.ReplyTo
	if len(sender) == 0 {
		sender = env.From
	}
	if len(replyTo) == 0 {
		replyTo = env.From
	}
	for _, list := range [][]imaplib.Address{env.From, sender, replyTo, env.To, env.Cc, env.Bcc} {
		b.WriteByte(' ')
		writeAddressList(&b, list)
	}
	b.WriteByte(' ')
	AppendNString(&b, joinMessageIDs(env.InReplyTo), len(env.InReplyTo) > 0)
	b.WriteByte(' ')
	AppendNString(&b, bracketed(env.MessageID), env.MessageID != "")
	return b.String()
}

// envelopeDate is the Date header as it was written, which is what the
// reference caches: the parsed time is not what a client is shown.
func envelopeDate(env *imaplib.Envelope) string {
	if env.Date.IsZero() {
		return ""
	}
	return env.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700")
}

// The reference caches the header's own form, angle brackets and all; the
// parsed form here carries them stripped, so they are put back on the way out
// and taken off on the way in.
func bracketed(id string) string {
	if id == "" || strings.HasPrefix(id, "<") {
		return id
	}
	return "<" + id + ">"
}

func unbracketed(id string) string {
	if strings.HasPrefix(id, "<") && strings.HasSuffix(id, ">") {
		return id[1 : len(id)-1]
	}
	return id
}

func joinMessageIDs(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, bracketed(id))
	}
	return strings.Join(out, " ")
}

func writeAddressList(b *strings.Builder, addrs []imaplib.Address) {
	if len(addrs) == 0 {
		b.WriteString("NIL")
		return
	}
	b.WriteByte('(')
	for _, a := range addrs {
		b.WriteByte('(')
		if a.Name == "" {
			b.WriteString("NIL")
		} else {
			AppendStringForHumans(b, a.Name)
		}
		// route is always NIL: RFC 5322 dropped source routes and the
		// reference writes what it parsed, which is nothing.
		b.WriteString(" NIL ")
		AppendNString(b, a.Mailbox, a.Mailbox != "")
		b.WriteByte(' ')
		AppendNString(b, a.Host, a.Host != "")
		b.WriteByte(')')
	}
	b.WriteByte(')')
}

// ParseEnvelope reads what WriteEnvelope wrote, and what the reference wrote.
// A malformation is (nil, false): a cache miss, never an error.
func ParseEnvelope(s string) (*imaplib.Envelope, bool) {
	p := &parser{in: s}
	items := make([]arg, 0, 10)
	for i := 0; i < 10; i++ {
		if i > 0 && !p.space() {
			return nil, false
		}
		a, ok := p.arg()
		if !ok {
			return nil, false
		}
		items = append(items, a)
	}
	if !p.eof() {
		return nil, false
	}
	return envelopeFromArgs(items)
}

// envelopeFromArgs builds the envelope from ten parsed items, which is also
// what a message/rfc822 part carries inside a body structure.
func envelopeFromArgs(items []arg) (*imaplib.Envelope, bool) {
	if len(items) < 10 {
		return nil, false
	}
	env := &imaplib.Envelope{}
	if items[0].present && !items[0].list {
		if t, ok := parseMessageDate(items[0].str); ok {
			env.Date = t
		}
	}
	env.Subject = items[1].str
	lists := []*[]imaplib.Address{&env.From, &env.Sender, &env.ReplyTo, &env.To, &env.Cc, &env.Bcc}
	for i, dst := range lists {
		addrs, ok := addressListFromArg(items[2+i])
		if !ok {
			return nil, false
		}
		*dst = addrs
	}
	for _, id := range strings.Fields(items[8].str) {
		env.InReplyTo = append(env.InReplyTo, unbracketed(id))
	}
	env.MessageID = unbracketed(items[9].str)
	return env, true
}

func addressListFromArg(a arg) ([]imaplib.Address, bool) {
	if !a.present {
		return nil, true
	}
	if !a.list {
		return nil, false
	}
	out := make([]imaplib.Address, 0, len(a.items))
	for _, item := range a.items {
		if !item.list || len(item.items) < 4 {
			return nil, false
		}
		out = append(out, imaplib.Address{
			Name:    item.items[0].str,
			Mailbox: item.items[2].str,
			Host:    item.items[3].str,
		})
	}
	return out, true
}

// EnvelopeHead is what ordering reads: the sent date, the base subject, and
// the first mailbox of three address lists (RFC 5256 §2.2).
type EnvelopeHead struct {
	Date         time.Time
	Subject      string
	From, To, Cc string
	InReplyTo    []string
	MessageID    string
}

// ParseEnvelopeHead reads the ordering fields without building the six address
// lists an envelope carries (#1490).
func ParseEnvelopeHead(s string) (EnvelopeHead, bool) {
	var h EnvelopeHead
	p := &parser{in: s}
	date, ok := p.arg()
	if !ok {
		return h, false
	}
	if date.present {
		if t, ok := parseMessageDate(date.str); ok {
			h.Date = t
		}
	}
	if !p.space() {
		return h, false
	}
	subject, ok := p.arg()
	if !ok {
		return h, false
	}
	h.Subject = subject.str
	// from, sender, reply-to, to, cc, bcc -- three of the six are read.
	for _, want := range []*string{&h.From, nil, nil, &h.To, &h.Cc, nil} {
		if !p.space() {
			return h, false
		}
		if want == nil {
			if !p.skip() {
				return h, false
			}
			continue
		}
		mailbox, ok := p.firstMailbox()
		if !ok {
			return h, false
		}
		*want = mailbox
	}
	if !p.space() {
		return h, false
	}
	inReplyTo, ok := p.arg()
	if !ok {
		return h, false
	}
	for _, id := range strings.Fields(inReplyTo.str) {
		h.InReplyTo = append(h.InReplyTo, unbracketed(id))
	}
	if !p.space() {
		return h, false
	}
	messageID, ok := p.arg()
	if !ok {
		return h, false
	}
	h.MessageID = unbracketed(messageID.str)
	return h, p.eof()
}
