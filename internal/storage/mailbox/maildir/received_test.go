package maildir

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// An APPEND date survives a rebuild from storage: Scan reads the published
// file's mtime, so naming the file must leave the INTERNALDATE on it (#2175).
func TestTheAppendDateSurvivesAScan(t *testing.T) {
	box, idx, folder := recSetup(t)
	when := time.Date(2024, 11, 23, 17, 45, 9, 0, time.UTC)
	const body = "From: a@b\r\n\r\nappended\r\n"
	saved, vsize, guid, err := box.SaveReceived("INBOX", strings.NewReader(body), 0, int64(len(body)), []string{`\Seen`}, nil, [16]byte{}, when)
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid, Flags: []string{`\Seen`}, InternalDate: when}
	if err := mailboxbase.RecordSaved(idx, box, folder.ID, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}

	recs, err := box.Scan("INBOX")
	if err != nil || len(recs) != 1 {
		t.Fatalf("scan = %v, err = %v", recs, err)
	}
	if !recs[0].InternalDate.Equal(when) {
		t.Errorf("a rebuild reads INTERNALDATE %s, want the APPEND date %s", recs[0].InternalDate.UTC(), when)
	}
	if _, err := os.Stat(box.folderPath("INBOX") + "/tmp/" + saved); !os.IsNotExist(err) {
		t.Errorf("the body is still in tmp/: %v", err)
	}
}

// A scan the moment the file lands in cur/ caches its date by name; that date
// must already be the APPEND date, or every later rebuild keeps the write time
// until a flag change renames the file (#2175).
func TestAScanAtPublishCachesTheAppendDate(t *testing.T) {
	box, idx, folder := recSetup(t)
	when := time.Date(2024, 11, 23, 17, 45, 9, 0, time.UTC)
	const body = "From: a@b\r\n\r\nappended\r\n"
	saved, vsize, guid, err := box.SaveReceived("INBOX", strings.NewReader(body), 0, int64(len(body)), []string{`\Seen`}, nil, [16]byte{}, when)
	if err != nil {
		t.Fatal(err)
	}
	var atPublish []mailbox.ScanRecord
	testAfterPublish = func() {
		testAfterPublish = nil
		atPublish, _ = box.Scan("INBOX")
	}
	defer func() { testAfterPublish = nil }()
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid, Flags: []string{`\Seen`}, InternalDate: when}
	if err := mailboxbase.RecordSaved(idx, box, folder.ID, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	if len(atPublish) != 1 {
		t.Fatalf("the scan in the window saw %d records, want 1: the row tests nothing", len(atPublish))
	}

	recs, err := box.Scan("INBOX")
	if err != nil || len(recs) != 1 {
		t.Fatalf("scan = %v, err = %v", recs, err)
	}
	if !recs[0].InternalDate.Equal(when) {
		t.Errorf("a rebuild after a scan at publish reads %s, want the APPEND date %s", recs[0].InternalDate.UTC(), when)
	}
}
