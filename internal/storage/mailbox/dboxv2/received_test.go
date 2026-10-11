package dboxv2

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// An APPEND date survives a rebuild from storage: Scan and List read R, and
// the named file carries the date as its mtime too (#2175).
func TestTheAppendDateSurvivesAScan(t *testing.T) {
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2024, 11, 23, 17, 45, 9, 0, time.UTC)
	const body = "From: a@b\r\n\r\nappended\r\n"
	saved, vsize, guid, err := mb.SaveReceived("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{}, when)
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid, InternalDate: when}
	if err := mailboxbase.RecordSaved(idx, mb, folder.ID, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}

	u := mb.(*userMailbox)
	named := filepath.Join(u.folderPath("INBOX"), "u.1")
	fi, err := os.Stat(named)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(when) {
		t.Errorf("the file's mtime is %s, want %s", fi.ModTime().UTC(), when)
	}
	// A copy or a restore from backup gives the file today's mtime; R is what
	// the date is read from.
	now := time.Now()
	if err := os.Chtimes(named, now, now); err != nil {
		t.Fatal(err)
	}

	recs, err := u.Scan("INBOX")
	if err != nil || len(recs) != 1 {
		t.Fatalf("scan = %v, err = %v", recs, err)
	}
	if !recs[0].InternalDate.Equal(when) {
		t.Errorf("Scan reads INTERNALDATE %s, want the APPEND date %s", recs[0].InternalDate.UTC(), when)
	}
	listed, err := u.List("INBOX")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %v, err = %v", listed, err)
	}
	if !listed[0].InternalDate.Equal(when) {
		t.Errorf("List reads INTERNALDATE %s, want %s", listed[0].InternalDate.UTC(), when)
	}
}
