package msgcache

import (
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/imaptext"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Head is the part of an envelope that ordering uses: SORT and THREAD compare
// the sent date, the base subject, and the FIRST MAILBOX of an address list --
// never a name, never a host, never a second address, and never Sender,
// Reply-To or Bcc at all.
//
// It exists because decoding the whole envelope for those five strings was
// 30.6% of every object a SORT (DATE) allocated on a ten-thousand-message
// account: six address lists nobody in that command would read (#1490). The
// measurement is on the issue; the number that justified this is a field one.
type Head struct {
	Date    time.Time
	Subject string
	// From, To and Cc carry the first address's mailbox part -- the local
	// part, which is what RFC 5256 §2.2 sorts an address key by.
	From, To, Cc string
	InReplyTo    []string
	MessageID    string
}

// decodeHead reads a Head out of the same bytes decodeEnvelope reads, skipping
// what it does not return.
//
// It walks the record rather than indexing into it: the fields are
// length-prefixed in sequence, so the only way to reach the message id is
// through the address lists. What it avoids is not the walk but the building
// -- a string per name, mailbox and host, and a slice per list.
func decodeHead(s string) (Head, bool) {
	h, ok := imaptext.ParseEnvelopeHead(s)
	if !ok {
		return Head{}, false
	}
	return Head{
		Date: h.Date, Subject: h.Subject,
		From: h.From, To: h.To, Cc: h.Cc,
		InReplyTo: h.InReplyTo, MessageID: h.MessageID,
	}, true
}

// Head returns the ordering fields of the cached envelope, or false on any of
// the three misses.
func (fc *Handle) Head(m *mailbox.MessageMeta) (Head, bool) {
	if fc == nil {
		return Head{}, false
	}
	data, ok := fc.read(m)[fc.ids[fieldIMAPEnvelope]]
	if !ok {
		return Head{}, false
	}
	return decodeHead(string(data))
}

// HeadAndReferences reads both in ONE pass over the message's record, for the
// same reason EnvelopeAndReferences does: asking separately walks the record
// chain twice, which was most of what a THREAD cost (#1461).
//
// The bool is "both halves are here", not "the head decoded". Threading needs
// the References as much as the envelope -- a head without them would thread
// the message by subject alone and put it in the wrong conversation -- so a
// caller that can only use the pair should not have to check two flags and
// remember which combination is safe.
func (fc *Handle) HeadAndReferences(m *mailbox.MessageMeta) (Head, []string, bool) {
	if fc == nil {
		return Head{}, nil, false
	}
	vals := fc.read(m)
	envData, ok := vals[fc.ids[fieldIMAPEnvelope]]
	if !ok {
		return Head{}, nil, false
	}
	h, ok := decodeHead(string(envData))
	if !ok {
		return Head{}, nil, false
	}
	refsData, cached := vals[fc.ids[fieldHdrReferences]]
	if !cached {
		return h, nil, false
	}
	refs, ok := referencesFromHeader(refsData)
	return h, refs, ok
}

// HeadOf takes the ordering fields off a freshly-parsed envelope, for the miss
// path that had to open the message.
//
// It exists so "which fields ordering uses" is written once. Spelling it again
// at the call site would let the two answers drift, and the drift would show as
// a mailbox that sorts differently depending on whether its cache was warm.
func HeadOf(env *imaplib.Envelope) Head {
	h := Head{
		Date:      env.Date,
		Subject:   env.Subject,
		InReplyTo: env.InReplyTo,
		MessageID: env.MessageID,
	}
	first := func(addrs []imaplib.Address) string {
		if len(addrs) == 0 {
			return ""
		}
		return addrs[0].Mailbox
	}
	h.From, h.To, h.Cc = first(env.From), first(env.To), first(env.Cc)
	return h
}
