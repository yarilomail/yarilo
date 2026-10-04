package imaptext_test

import (
	"bufio"
	"os"
	"testing"

	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	"github.com/yarilomail/yarilo/internal/imaptext"
)

// rawFileHeader reads the fixture's header without decoding anything.
type rawFileHeader struct{ h textproto.Header }

func (r rawFileHeader) RawValues(name string) []string {
	var out []string
	fields := r.h.FieldsByKey(name)
	for fields.Next() {
		out = append(out, fields.Value())
	}
	return out
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Trap (d): the envelope and the body structure this package writes for the
// crafted message are the bytes a reference install cached for it
// (testdata/README.md has the recipe). The message is awkward on purpose:
// encoded words, a quoted local part, an empty group, a folded header and a
// childless multipart.
func TestTheFixtureMessageIsWrittenAsTheReferenceWroteIt(t *testing.T) {
	f, err := os.Open("testdata/crafted.eml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck

	hdr, err := textproto.ReadHeader(bufio.NewReader(f))
	if err != nil {
		t.Fatal(err)
	}
	if got := imaptext.EnvelopeFromHeader(rawFileHeader{h: hdr}); got != fixture(t, "reference-envelope.txt") {
		t.Errorf("envelope\n  %q\nreference\n  %q", got, fixture(t, "reference-envelope.txt"))
	}

	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	bs := imapserver.ExtractBodyStructure(f)
	got, ok := imaptext.WriteBodyStructure(bs, true)
	if !ok {
		t.Fatal("the fixture's body structure was refused")
	}
	if want := fixture(t, "reference-bodystructure.txt"); got != want {
		t.Errorf("body structure\n  %q\nreference\n  %q", got, want)
	}
}
