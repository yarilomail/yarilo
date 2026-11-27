package mdbox

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The reference fixture's third record is 67 bytes on disk with bare LF; the
// raw read returns those, where Fetch serves 72 CRLF.
func TestRawEntryKeepsBareLF(t *testing.T) {
	u := &userMailbox{home: t.TempDir()}
	dir := u.storagePath()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := dboxref.MdboxFile(t)
	if err := os.WriteFile(filepath.Join(dir, "m.1"), file, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := u.rawEntry(mdboxmap.MapEntry{FileID: 1, Offset: 4690}, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(io.NewSectionReader(raw, 0, raw.Size()))
	_ = raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 67 || bytes.Contains(got, []byte("\r\n")) || !bytes.Contains(file, got) {
		t.Fatalf("raw body %d bytes %q, want the 67 stored with bare LF", len(got), got)
	}
}

// An expunged, unpurged record is still on disk, and RawUnreferenced reaches
// it: a status that left it out would report a zero under the old key falsely.
func TestRawUnreferencedReachesExpungedRecords(t *testing.T) {
	mb, _ := newTestUser(t)
	var names []string
	for i, body := range []string{"kept\r\n", "gone\r\n"} {
		n, _, _, err := mb.Save("INBOX", strings.NewReader(body), uint32(i+1), int64(len(body)), nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := mb.Remove("INBOX", names[1]); err != nil {
		t.Fatal(err)
	}
	var got []string
	err := mb.(*userMailbox).RawUnreferenced(func(_ uint32, b mailbox.RawBody) error {
		body, err := io.ReadAll(io.NewSectionReader(b, 0, b.Size()))
		got = append(got, string(body))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "gone\r\n" {
		t.Fatalf("unreferenced %q, want the removed message only", got)
	}
}
