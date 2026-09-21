package imaptext

import (
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

// The bytes are the contract: a cache written here is read by the reference.
// The wanted strings are the reference's own writer applied by hand to each
// input (imap-envelope.c:47-87, imap-quote.c:45-240).
func TestWriteEnvelopeMatchesTheReferencesBytes(t *testing.T) {
	when := time.Date(2026, 9, 21, 14, 30, 5, 0, time.FixedZone("", 2*3600))
	tests := []struct {
		name string
		env  *imaplib.Envelope
		want string
	}{
		{
			name: "nothing at all",
			env:  nil,
			want: "NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL",
		},
		{
			name: "a plain message",
			env: &imaplib.Envelope{
				Date:      when,
				Subject:   "hello",
				From:      []imaplib.Address{{Name: "Ann", Mailbox: "ann", Host: "example.com"}},
				To:        []imaplib.Address{{Mailbox: "bo", Host: "example.org"}},
				MessageID: "<1@example.com>",
			},
			want: `"Mon, 21 Sep 2026 14:30:05 +0200" "hello" (("Ann" NIL "ann" "example.com")) (("Ann" NIL "ann" "example.com")) (("Ann" NIL "ann" "example.com")) ((NIL NIL "bo" "example.org")) NIL NIL NIL "<1@example.com>"`,
		},
		{
			name: "a subject that has to become a literal",
			env:  &imaplib.Envelope{Subject: "a\tb"},
			want: "NIL {3}\r\na b NIL NIL NIL NIL NIL NIL NIL NIL",
		},
		{
			name: "eight-bit in a display name",
			env: &imaplib.Envelope{
				From: []imaplib.Address{{Name: "Ганна", Mailbox: "h", Host: "e.ua"}},
			},
			want: "NIL NIL (({10}\r\nГанна NIL \"h\" \"e.ua\")) (({10}\r\nГанна NIL \"h\" \"e.ua\")) (({10}\r\nГанна NIL \"h\" \"e.ua\")) NIL NIL NIL NIL NIL",
		},
		{
			name: "a message-id with a quote in it stays quoted",
			env:  &imaplib.Envelope{MessageID: `<a"b@c>`},
			want: "NIL NIL NIL NIL NIL NIL NIL NIL NIL \"<a\\\"b@c>\"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := WriteEnvelope(tc.env); got != tc.want {
				t.Errorf("wrote\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// What we write we must read back: the cache is written once and read on every
// listing, so a codec that cannot round-trip costs an open per message.
func TestEnvelopeRoundTrip(t *testing.T) {
	when := time.Date(2026, 9, 21, 14, 30, 5, 0, time.FixedZone("", 2*3600))
	env := &imaplib.Envelope{
		Date:      when,
		Subject:   "Ганна пише",
		From:      []imaplib.Address{{Name: "Ann Lee", Mailbox: "ann", Host: "example.com"}},
		To:        []imaplib.Address{{Mailbox: "bo", Host: "example.org"}, {Mailbox: "cy", Host: "example.net"}},
		Cc:        []imaplib.Address{{Name: "C C", Mailbox: "cc", Host: "example.com"}},
		InReplyTo: []string{"<0@example.com>"},
		MessageID: "<1@example.com>",
	}
	got, ok := ParseEnvelope(WriteEnvelope(env))
	if !ok {
		t.Fatal("what we wrote did not parse")
	}
	if !got.Date.Equal(env.Date) {
		t.Errorf("date = %v, want %v", got.Date, env.Date)
	}
	if got.Subject != env.Subject {
		t.Errorf("subject = %q, want %q", got.Subject, env.Subject)
	}
	if len(got.To) != 2 || got.To[1].Mailbox != "cy" || got.To[1].Host != "example.net" {
		t.Errorf("to = %+v", got.To)
	}
	if len(got.Cc) != 1 || got.Cc[0].Name != "C C" {
		t.Errorf("cc = %+v", got.Cc)
	}
	if len(got.InReplyTo) != 1 || got.InReplyTo[0] != "<0@example.com>" {
		t.Errorf("in-reply-to = %v", got.InReplyTo)
	}
	if got.MessageID != env.MessageID {
		t.Errorf("message-id = %q", got.MessageID)
	}
}

// A cache the reference wrote is the input we cannot control: it has to be
// read as it is, literals and all.
func TestParseEnvelopeWrittenByTheReference(t *testing.T) {
	const s = "\"Fri, 18 Sep 2026 09:12:00 +0300\" {13}\r\nPrivet, svit! ((\"Ivan\" NIL \"ivan\" \"example.ua\")) NIL NIL ((NIL NIL \"list\" \"example.org\")) NIL NIL NIL \"<abc@example.ua>\""
	env, ok := ParseEnvelope(s)
	if !ok {
		t.Fatal("a reference-written envelope did not parse")
	}
	if env.Subject != "Privet, svit!" {
		t.Errorf("subject = %q", env.Subject)
	}
	if len(env.From) != 1 || env.From[0].Name != "Ivan" || env.From[0].Mailbox != "ivan" {
		t.Errorf("from = %+v", env.From)
	}
	if len(env.Sender) != 0 || len(env.ReplyTo) != 0 {
		t.Errorf("NIL sender/reply-to became %+v / %+v", env.Sender, env.ReplyTo)
	}
	if env.MessageID != "<abc@example.ua>" {
		t.Errorf("message-id = %q", env.MessageID)
	}
	if env.Date.IsZero() {
		t.Error("the date was dropped")
	}
}
