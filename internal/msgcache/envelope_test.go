package msgcache

import (
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/imaptext"
)

// Round-trip with inputs that distinguish: UTF-8 in names and subject, an
// empty address list beside a populated one, several recipients, a zero date.
// The encoding is the reference's, so sender and reply-to come back filled
// from From -- that is what a reader of theirs sees too.
func TestEnvelopeRoundTripThroughTheReferenceEncoding(t *testing.T) {
	cases := []*imaplib.Envelope{
		{
			Date:    time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
			Subject: "Тема з UTF-8 — і тире",
			From:    []imaplib.Address{{Name: "Аліса Ліддел", Mailbox: "alice", Host: "example.com"}},
			To: []imaplib.Address{
				{Mailbox: "bob", Host: "example.com"},
				{Name: "Carol", Mailbox: "carol", Host: "example.org"},
			},
			InReplyTo: []string{"<a@x>", "<b@y>"},
			MessageID: "<m1@example.com>",
		},
		{}, // everything empty, date zero
		{Subject: "only-subject", Bcc: []imaplib.Address{{Mailbox: "hidden", Host: "h"}}},
	}
	for i, env := range cases {
		got, ok := imaptext.ParseEnvelope(imaptext.WriteEnvelope(env))
		if !ok {
			t.Fatalf("case %d: the encoding did not parse", i)
		}
		if !env.Date.IsZero() && !got.Date.Equal(env.Date) {
			t.Errorf("case %d: date %v != %v", i, got.Date, env.Date)
		}
		if got.Subject != env.Subject {
			t.Errorf("case %d: subject %q != %q", i, got.Subject, env.Subject)
		}
		if len(got.To) != len(env.To) || len(got.Bcc) != len(env.Bcc) {
			t.Errorf("case %d: to/bcc counts %d/%d, want %d/%d", i, len(got.To), len(got.Bcc), len(env.To), len(env.Bcc))
		}
		if got.MessageID != env.MessageID {
			t.Errorf("case %d: message-id %q != %q", i, got.MessageID, env.MessageID)
		}
		if len(env.From) > 0 && (len(got.Sender) != 1 || got.Sender[0].Mailbox != env.From[0].Mailbox) {
			t.Errorf("case %d: sender %+v does not fall back to From, as the reference writes it", i, got.Sender)
		}
	}
}

// Malformation is a miss, never a panic and never an error surfaced upward.
func TestAMalformedEnvelopeIsAMiss(t *testing.T) {
	good := imaptext.WriteEnvelope(&imaplib.Envelope{Subject: "s", MessageID: "<a@b>"})
	for _, s := range []string{
		"",
		"NIL",                       // too few items
		good + " NIL",               // too many
		good[:len(good)-2],          // truncated
		`"unterminated NIL NIL NIL`, // an open quote
		"NIL NIL {99}\r\nshort NIL NIL NIL NIL NIL NIL NIL", // a literal longer than what follows
	} {
		if env, ok := imaptext.ParseEnvelope(s); ok {
			t.Errorf("malformed %q parsed to %+v", s, env)
		}
	}
}
