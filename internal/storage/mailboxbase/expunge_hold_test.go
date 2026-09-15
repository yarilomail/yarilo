package mailboxbase_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// An open between the record and the body finds a file no record names, so the
// expunge holds the folder and the open waits (#1794).
func TestAnOpenCannotEnterTheExpungeWindow(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: home, Driver: "maildir"}
	lk := &countingLocker{held: map[string]locks.HoldMode{}}
	store := maildir.New().OpenUser(info)
	idx := fileidx.New(fileidx.WithLocker(lk)).OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mailboxbase.SetTestSyncTokens(8))

	box := mailboxbase.Open(store, idx)
	f, err := box.Folder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := idx.AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	const body = "From: a@b\r\nSubject: window\r\n\r\nbody\r\n"
	saved, vsize, guid, err := store.Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := box.RecordDelivered(f, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}

	// The second session, which is what a reconcile rides in on.
	other := mailboxbase.Open(store, idx)
	entered := make(chan struct{})
	var opened sync.WaitGroup
	opened.Add(1)
	restore := mailboxbase.SetTestAfterRecordExpunged(func() {
		close(entered)
		// Long enough that an unheld open would finish inside the window.
		time.Sleep(200 * time.Millisecond)
	})
	t.Cleanup(restore)

	go func() {
		defer opened.Done()
		<-entered
		if _, oerr := other.Folder("INBOX", 1); oerr != nil {
			t.Errorf("second open: %v", oerr)
		}
	}()

	removed, failed, _ := box.ExpungeMarked(f, "INBOX", []*mailbox.MessageMeta{m}, nil)
	if len(removed) != 1 || failed != 0 {
		t.Fatalf("expunge removed %v, failed %d", removed, failed)
	}
	opened.Wait()

	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("the folder holds %d records after the expunge, want none: the open resurrected it", len(msgs))
	}
	ents, rerr := os.ReadDir(filepath.Join(home, "Maildir", "cur"))
	if rerr != nil {
		t.Fatalf("cur/: %v", rerr)
	}
	if len(ents) != 0 {
		t.Errorf("cur/ holds %d files after the expunge, want none", len(ents))
	}
}
