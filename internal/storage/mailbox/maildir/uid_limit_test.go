package maildir_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A list at the top of the uid space is adopted up to its last uid, 2^32-2; a
// row at 2^32-1 is a corrupt one, as the reference reads it; and the delivery
// after the last uid is refused, never handed a uid that wrapped to 1.
func TestAListAtTheTopOfTheUIDSpace(t *testing.T) {
	home := t.TempDir()
	cur := filepath.Join(home, "Maildir", "cur")
	if err := os.MkdirAll(cur, 0o700); err != nil {
		t.Fatal(err)
	}
	files := []string{"1700000001.M1P1.host,S=14:2,S", "1700000002.M2P2.host,S=14:2,", "1700000003.M3P3.host,S=14:2,"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(cur, name), []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The header trails the rows, as after rows appended blind: the next uid is
	// read past it, and a row at 2^32-1 must not wrap it to zero.
	list := fmt.Sprintf("3 V1600000000 N2\n1 :%s\n%d :%s\n%d :%s\n",
		"1700000001.M1P1.host,S=14",
		uint32(mailbox.MaxUID), "1700000002.M2P2.host,S=14",
		uint64(mailbox.MaxUID)+1, "1700000003.M3P3.host,S=14")
	if err := os.WriteFile(filepath.Join(home, "Maildir", "dovecot-uidlist"), []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	syncer := mailbox.Driver(box).(interface {
		ReconcileIndex(mailbox.Box, mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	if _, err := syncer.ReconcileIndex(mailboxbase.Open(box, idx), idx, f); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint32]bool{}
	for _, m := range msgs {
		got[m.UID] = true
	}
	if !got[1] || !got[mailbox.MaxUID] {
		t.Fatalf("the folder holds %v; want 1 and %d at the uids the list gives", got, uint32(mailbox.MaxUID))
	}
	if len(got) != 2 {
		t.Errorf("the folder holds %d records; the file of the corrupt row has no uid left to take: %v", len(got), got)
	}
	const body = "From: a@b\r\n\r\nnew\r\n"
	saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); !errors.Is(err, mailbox.ErrUIDSpaceFull) {
		t.Fatalf("a delivery past the last uid: uid %d, %v; want the space full", m.UID, err)
	}
}
