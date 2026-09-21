package msgcache

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-message/textproto"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const craftedHeaderText = "Date: Mon, 21 Sep 2026 14:05:06 +0300\r\n" +
	"Subject: =?UTF-8?Q?Caf=C3=A9_menu?= \"quoted\" back\\slash\r\n" +
	"From: \"Doe, John\" <john@example.test>\r\n" +
	"Reply-To: <reply@example.test>\r\n" +
	"To: =?UTF-8?B?0IbQstCw0L0=?= <ivan@example.test>, \"quoted local\"@example.test\r\n" +
	"Cc: undisclosed-recipients:;\r\n" +
	"In-Reply-To: <m0@example.test>\r\n" +
	"Message-Id: <m1@example.test>\r\n" +
	"\r\n"

func craftedHeader(t *testing.T) textproto.Header {
	t.Helper()
	h, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(craftedHeaderText)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A record holding the headers but no built envelope is what a cache from the
// reference looks like: the envelope is built from them and written back, so
// the next read is a plain hit (index-mail-headers.c:515-560).
func TestAnEnvelopeIsBuiltFromCachedHeaders(t *testing.T) {
	idx, f, m := compatFolder(t)
	hdr := craftedHeader(t)

	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.storeEnvelopeHeaders(m, hdr)
	fc.Close()

	m = reread(t, idx, f.ID, m.UID)
	reader := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if reader == nil {
		t.Fatal("cache unavailable")
	}
	if _, ok := reader.read(m)[reader.fieldID(fieldIMAPEnvelope)]; ok {
		t.Fatal("the record already carries an envelope, so this proves nothing")
	}
	text, ok := reader.EnvelopeText(m)
	if !ok {
		t.Fatal("no envelope was built from the cached headers")
	}
	if want := EnvelopeTextOf(hdr); text != want {
		t.Errorf("built\n  %q\nwant\n  %q", text, want)
	}
	reader.Close()

	// And it was written back, so the next reader does not build it again.
	m = reread(t, idx, f.ID, m.UID)
	second := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if second == nil {
		t.Fatal("cache unavailable")
	}
	defer second.Close()
	if _, ok := second.read(m)[second.fieldID(fieldIMAPEnvelope)]; !ok {
		t.Error("the built envelope was not written back as imap.envelope")
	}
}

// A field table written elsewhere spells the same header two ways; the lookup
// has to be the reference's, which hashes names case-insensitively
// (mail-cache.c:575-576).
func TestACachedHeaderIsFoundWhateverItsCase(t *testing.T) {
	idx, f, m := compatFolder(t)
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	// The spelling the fixture from the reference carries.
	id, ok := fc.file.FieldID("hdr.MESSAGE-ID")
	if !ok {
		t.Fatal("hdr.Message-ID is not registered at all")
	}
	if want := fc.fieldID(headerField("Message-ID")); id != want {
		t.Errorf("hdr.MESSAGE-ID resolved to %d, hdr.Message-ID is %d", id, want)
	}
	fc.storeField(m, id, encodeHeaderField(1, "Message-ID: <m1@example.test>\r\n"))
	fc.Close()

	m = reread(t, idx, f.ID, m.UID)
	reader := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if reader == nil {
		t.Fatal("cache unavailable")
	}
	defer reader.Close()
	if got := (cachedHeaders{fc: reader, vals: reader.read(m)}).RawValues("message-id"); len(got) != 1 {
		t.Errorf("the header stored under one spelling was not found under another: %v", got)
	}
}

// One parse of a header fills what the reference's cache holds after one FETCH
// ENVELOPE: the envelope, the ten headers, and the record's own fields.
func TestOneParseFillsTheHeadersToo(t *testing.T) {
	idx, f, m := compatFolder(t)
	hdr := craftedHeader(t)

	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreFromHeader(m, hdr, EnvelopeTextOf(hdr))
	fc.Close()

	m = reread(t, idx, f.ID, m.UID)
	reader := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if reader == nil {
		t.Fatal("cache unavailable")
	}
	defer reader.Close()
	vals := reader.read(m)
	for _, name := range []string{"Date", "Subject", "From", "Reply-To", "To", "Cc", "In-Reply-To", "Message-ID"} {
		if _, ok := vals[reader.fieldID(headerField(name))]; !ok {
			t.Errorf("hdr.%s was not cached; a reader of theirs would open the message for it", name)
		}
	}
	if _, ok := vals[reader.fieldID(fieldIMAPEnvelope)]; !ok {
		t.Error("imap.envelope was not cached")
	}
}

// The checksum still covers a record filled this way, or every read of it is a
// mismatch.
func TestARecordWithHeadersKeepsItsChecksum(t *testing.T) {
	idx, f, m := compatFolder(t)
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	hdr := craftedHeader(t)
	fc.StoreFromHeader(m, hdr, EnvelopeTextOf(hdr))
	fc.Close()

	m = reread(t, idx, f.ID, m.UID)
	if m.CacheCRC == 0 {
		t.Fatal("no checksum was stamped")
	}
	was := testutil.ToFloat64(metricCRCMismatch)
	reader := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if reader == nil {
		t.Fatal("cache unavailable")
	}
	defer reader.Close()
	if _, ok := reader.EnvelopeText(m); !ok {
		t.Error("the record was refused")
	}
	if now := testutil.ToFloat64(metricCRCMismatch); now != was {
		t.Errorf("a record written in eleven parts failed its own checksum: %v -> %v", was, now)
	}
}

var _ = mailbox.MessageMeta{}
