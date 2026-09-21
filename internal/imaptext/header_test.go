package imaptext

import (
	"strings"
	"testing"
)

// rawHeader is a header as read off the message, values undecoded.
type rawHeader map[string][]string

func (h rawHeader) RawValues(name string) []string {
	out := append([]string(nil), h[name]...)
	return out
}

// The bytes a reference install wrote for this message, read back out of its
// own cache (2.4.5, mdbox; the recipe is in testdata/README.md).
const referenceEnvelope = "\"Mon, 21 Sep 2026 14:05:06 +0300\" {46}\r\n" +
	"=?UTF-8?Q?Caf=C3=A9_menu?= \"quoted\" back\\slash " +
	`(("Doe, John" NIL "john" "example.test")) (("Doe, John" NIL "john" "example.test")) ((NIL NIL "reply" "example.test")) (("=?UTF-8?B?0IbQstCw0L0=?=" NIL "ivan" "example.test")(NIL NIL "quoted local" "example.test")) ((NIL NIL "undisclosed-recipients" NIL)(NIL NIL NIL NIL)) NIL "<m0@example.test>" "<m1@example.test>"`

// craftedHeader is the message those bytes came from: a Q-encoded subject
// carrying a quote and a backslash, a B-encoded display name, a quoted local
// part, an empty group, and a folded References.
func craftedHeader() rawHeader {
	return rawHeader{
		"Date":         {"Mon, 21 Sep 2026 14:05:06 +0300"},
		"Subject":      {`=?UTF-8?Q?Caf=C3=A9_menu?= "quoted" back\slash`},
		"From":         {`"Doe, John" <john@example.test>`},
		"Reply-To":     {"<reply@example.test>"},
		"To":           {`=?UTF-8?B?0IbQstCw0L0=?= <ivan@example.test>, "quoted local"@example.test`},
		"Cc":           {"undisclosed-recipients:;"},
		"In-Reply-To":  {"<m0@example.test>"},
		"Message-Id":   {"<m1@example.test>"},
		"References":   {"<m0@example.test>\r\n <mminus1@example.test>"},
		"Content-Type": {"multipart/mixed; boundary=b1"},
	}
}

// The envelope the cache holds must be the envelope the reference would write
// for the same message: undecoded, group and all (#1714).
func TestEnvelopeFromHeaderMatchesTheReference(t *testing.T) {
	if got := EnvelopeFromHeader(craftedHeader()); got != referenceEnvelope {
		t.Errorf("built\n  %q\nreference wrote\n  %q", got, referenceEnvelope)
	}
}

// And what we build parses back: the same text serves SORT and THREAD.
func TestTheBuiltEnvelopeIsReadable(t *testing.T) {
	env, ok := ParseEnvelope(EnvelopeFromHeader(craftedHeader()))
	if !ok {
		t.Fatal("the envelope we build does not parse")
	}
	if len(env.From) != 1 || env.From[0].Name != "Doe, John" || env.From[0].Mailbox != "john" {
		t.Errorf("from = %+v", env.From)
	}
	if len(env.To) != 2 || env.To[1].Mailbox != "quoted local" {
		t.Errorf("to = %+v", env.To)
	}
	if env.MessageID != "m1@example.test" {
		t.Errorf("message-id = %q", env.MessageID)
	}
	if !strings.Contains(env.Subject, "=?UTF-8?Q?") {
		t.Errorf("subject = %q: an encoded word was decoded somewhere", env.Subject)
	}
}

// Repeated headers merge comma-separated, as the reference merges them: a
// reader must not be shown one From while another line validated the other.
func TestRepeatedHeadersMerge(t *testing.T) {
	h := rawHeader{"From": {"<a@x.test>", "<b@y.test>"}}
	got := EnvelopeFromHeader(h)
	if !strings.Contains(got, `((NIL NIL "a" "x.test")(NIL NIL "b" "y.test"))`) {
		t.Errorf("two From lines became %q", got)
	}
}
