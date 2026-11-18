package dboxv2

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A reference file's body comes back from RawRecord as stored, bare LF and
// all, where Fetch serves it CRLF.
func TestRawRecordKeepsBareLF(t *testing.T) {
	_, mb, _ := newTestUser(t)
	u := mb.(*userMailbox)
	path := filepath.Join(u.folderPath("INBOX"), "u.1")
	if err := os.WriteFile(path, dboxref.SdboxShort(t), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := u.RawRecord("INBOX", &mailbox.MessageMeta{UID: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(io.NewSectionReader(raw, 0, raw.Size()))
	_ = raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 63 || !bytes.Contains(got, []byte("\n")) || bytes.Contains(got, []byte("\r\n")) {
		t.Fatalf("raw body %d bytes %q, want the 63 stored with bare LF", len(got), got)
	}
	_, sec, err := openBody(path)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := io.ReadAll(sec)
	if !bytes.Equal(got, want) {
		t.Fatal("raw body differs from the file's body section")
	}
}
