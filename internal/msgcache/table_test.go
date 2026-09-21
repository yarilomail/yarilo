package msgcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/imaptext"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// Trap (c): the field table a folder ends up with names only fields the
// reference has. A name of ours in it is a cache only we can read, which is
// the whole of what #1714 removes -- so the table is read off the file rather
// than off the list in the source.
func TestTheFieldTableNamesNothingOfOurs(t *testing.T) {
	idx, f, m := compatFolder(t)
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreEnvelope(m, &imaplib.Envelope{Subject: "Plan", MessageID: "a@x"})
	fc.StoreReferences(m, []string{"<root@x>"})
	fc.StoreBodyStructure(m, &imaplib.BodyStructureSinglePart{
		Type: "text", Subtype: "plain", Encoding: "7bit", Size: 3,
		Text: &imaplib.BodyStructureText{NumLines: 1},
	})
	fc.StoreSizes(m, 100, 110)
	fc.Close()

	ic, ok := idx.(Index)
	if !ok {
		t.Fatal("the index serves no cache")
	}
	path, err := ic.CachePath(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	indexID, resetID, _, err := ic.CachePairIdentity(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := mailindex.OpenCache(path, indexID, resetID)
	if err != nil {
		t.Fatalf("open the cache we just wrote: %v", err)
	}
	defer cf.Close() //nolint:errcheck

	known := make(map[string]bool, len(referenceFields))
	for _, f := range referenceFields {
		known[f.Name] = true
	}
	var names []string
	for _, field := range cf.Fields() {
		names = append(names, field.Name)
		if strings.HasPrefix(field.Name, "yarilo.") {
			t.Errorf("the file names %q, which no other implementation can read", field.Name)
		}
		if !known[field.Name] {
			t.Errorf("the file names %q, which is not in the reference's table", field.Name)
		}
	}
	if len(names) == 0 {
		t.Fatal("the file names no fields at all, so this asserts nothing")
	}
}

// Trap (d): the bytes of one record. The values are read back out of the file
// and compared against what the reference's writers produce for the same
// message (imap-envelope.c:47-87, imap-bodystructure.c:26-283), so a change to
// either encoding shows up here as bytes, not as a round trip agreeing with
// itself.
func TestOneRecordsBytesAreTheReferencesBytes(t *testing.T) {
	idx, f, m := compatFolder(t)
	env := &imaplib.Envelope{
		Subject:   "a listing",
		From:      []imaplib.Address{{Name: "Ann", Mailbox: "ann", Host: "example.com"}},
		To:        []imaplib.Address{{Name: "Bo", Mailbox: "bo", Host: "example.org"}},
		MessageID: "listing@example.com",
	}
	bs := &imaplib.BodyStructureSinglePart{
		Type: "text", Subtype: "plain", Params: map[string]string{"charset": "utf-8"},
		Encoding: "7bit", Size: 18, Text: &imaplib.BodyStructureText{NumLines: 2},
	}

	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreEnvelope(m, env)
	fc.StoreBodyStructure(m, bs)
	fc.StoreSizes(m, 18, 18)
	fc.StoreReferences(m, []string{"<root@example.com>"})
	fc.Close()

	m = reread(t, idx, f.ID, m.UID)
	fresh := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fresh == nil {
		t.Fatal("cache unavailable")
	}
	defer fresh.Close()
	vals := fresh.read(m)
	if len(vals) == 0 {
		t.Fatal("the record read back empty")
	}

	const wantEnvelope = `NIL "a listing" (("Ann" NIL "ann" "example.com")) (("Ann" NIL "ann" "example.com")) (("Ann" NIL "ann" "example.com")) (("Bo" NIL "bo" "example.org")) NIL NIL NIL "<listing@example.com>"`
	const wantBodyStructure = `"text" "plain" ("charset" "utf-8") NIL NIL "7bit" 18 2 NIL NIL NIL NIL`
	const wantBody = `"text" "plain" ("charset" "utf-8") NIL NIL "7bit" 18 2`

	for _, tc := range []struct {
		field string
		want  string
	}{
		{fieldIMAPEnvelope, wantEnvelope},
		{fieldIMAPBodyStructure, wantBodyStructure},
		{fieldIMAPBody, wantBody},
	} {
		if got := string(vals[fresh.ids[tc.field]]); got != tc.want {
			t.Errorf("%s on disk is\n  %q\nwant\n  %q", tc.field, got, tc.want)
		}
	}

	// The fixed-size fields are the reference's widths, little-endian as it
	// writes them on the platforms this runs on.
	if got := vals[fresh.ids[fieldSizePhysical]]; len(got) != 8 {
		t.Errorf("size.physical is %d bytes, want 8", len(got))
	}
	if got, ok := decodeU64(vals[fresh.ids[fieldSizeVirtual]]); !ok || got != 18 {
		t.Errorf("size.virtual = %v (%v)", got, ok)
	}
	// The header field keeps the whole line, line numbers first, as the
	// reference stores a cached header (index-mail-headers.c:99-127).
	line, ok := decodeHeaderField(vals[fresh.ids[fieldHdrReferences]])
	if !ok || line != "References: <root@example.com>\r\n" {
		t.Errorf("hdr.references = %q (%v)", line, ok)
	}

	// And what we wrote is what the writers produce, so neither side of the
	// comparison is this test's own opinion.
	if got := imaptext.WriteEnvelope(env); got != wantEnvelope {
		t.Errorf("the writer no longer produces the bytes this record holds:\n  %q", got)
	}
}

// A cache file belongs to one pair. The trap is here so that a fixture from
// elsewhere, when one exists, can be dropped in beside it.
func TestACacheFileOfAnotherPairIsNotRead(t *testing.T) {
	idx, f, m := compatFolder(t)
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreEnvelope(m, &imaplib.Envelope{Subject: "Plan"})
	fc.Close()

	ic := idx.(Index)
	path, err := ic.CachePath(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "yarilo.index.cache")
	if err := os.WriteFile(other, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	indexID, resetID, _, err := ic.CachePairIdentity(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mailindex.OpenCache(other, indexID+1, resetID); err == nil {
		t.Error("a cache file was read for an index it does not belong to")
	}
}
