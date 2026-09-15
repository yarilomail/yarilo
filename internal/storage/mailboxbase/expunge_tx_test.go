package mailboxbase_test

import (
	"fmt"
	"strings"
	"testing"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// End to end an EXPUNGE takes the folder once, and did before the transaction
// too: the driver's hold is outer, so nested index writes are reentrant.
func TestExpungingManyTakesTheFolderOnce(t *testing.T) {
	const messages = 40

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

	doomed := make([]*mailbox.MessageMeta, 0, messages)
	for i := 0; i < messages; i++ {
		uid, aerr := idx.AllocateUID(f.ID)
		if aerr != nil {
			t.Fatal(aerr)
		}
		body := fmt.Sprintf("From: a@b\r\nSubject: m%d\r\n\r\nbody\r\n", i)
		saved, vsize, guid, serr := store.Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
		if rerr := box.RecordDelivered(f, "INBOX", saved, m); rerr != nil {
			t.Fatal(rerr)
		}
		doomed = append(doomed, m)
	}

	before := journalHolds(t)
	removed, failed, nerr := box.ExpungeMarked(f, "INBOX", doomed)
	if nerr != nil || failed != 0 || len(removed) != messages {
		t.Fatalf("expunged %d, failed %d, err %v", len(removed), failed, nerr)
	}
	took := journalHolds(t) - before
	t.Logf("locks taken for %d messages: %d", messages, took)

	if took > 4 {
		t.Errorf("expunging %d messages took %d locks, want the command's own few", messages, took)
	}

	left, lerr := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(left) != 0 {
		t.Errorf("%d records survived the expunge", len(left))
	}
}

// The index on its own, where the count is actually paid: an outer folder hold
// makes both shapes read as one acquisition and proves nothing (#1827).
var perMessage = false

func TestTheIndexIsTakenOncePerTransaction(t *testing.T) {
	const messages = 40

	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: home, Driver: "maildir"}
	lk := &countingLocker{held: map[string]locks.HoldMode{}}
	idx := fileidx.New(fileidx.WithLocker(lk)).OpenUser(info)
	t.Cleanup(func() { _ = idx.Close() })

	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	uids := make([]uint32, 0, messages)
	for i := 0; i < messages; i++ {
		m := &mailbox.MessageMeta{Size: 10, VSize: 10}
		if aerr := idx.AllocateAndAppend(f.ID, m); aerr != nil {
			t.Fatal(aerr)
		}
		uids = append(uids, m.UID)
	}

	tx, berr := idx.Begin(f.ID)
	if berr != nil {
		t.Fatalf("begin: %v", berr)
	}
	before := journalHolds(t)
	if perMessage {
		for _, uid := range uids {
			if eerr := idx.ExpungeMessage(f.ID, uid); eerr != nil {
				t.Fatal(eerr)
			}
		}
		tx.Rollback()
	} else {
		for _, uid := range uids {
			tx.Expunge(uid)
		}
		if _, cerr := tx.Commit(); cerr != nil {
			t.Fatal(cerr)
		}
	}
	took := journalHolds(t) - before
	t.Logf("index locks for %d records in one transaction: %d", messages, took)
	if took != 1 {
		t.Errorf("one transaction over %d records took %d index locks, want 1", messages, took)
	}

	left, lerr := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(left) != 0 {
		t.Errorf("%d records survived the transaction", len(left))
	}
}

// A command's worth of flag changes is one transaction, so a STORE over N
// messages takes the index once (#1827).
func TestFlagChangesTakeTheIndexOncePerTransaction(t *testing.T) {
	const messages = 40

	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: home, Driver: "maildir"}
	lk := &countingLocker{held: map[string]locks.HoldMode{}}
	idx := fileidx.New(fileidx.WithLocker(lk)).OpenUser(info)
	t.Cleanup(func() { _ = idx.Close() })

	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	uids := make([]uint32, 0, messages)
	for i := 0; i < messages; i++ {
		m := &mailbox.MessageMeta{Size: 10, VSize: 10}
		if aerr := idx.AllocateAndAppend(f.ID, m); aerr != nil {
			t.Fatal(aerr)
		}
		uids = append(uids, m.UID)
	}

	tx, berr := idx.Begin(f.ID)
	if berr != nil {
		t.Fatal(berr)
	}
	before := journalHolds(t)
	for _, uid := range uids {
		tx.UpdateFlags(uid, mailbox.FlagsUpdate{Mode: mailbox.FlagsSet, Flags: []string{`\Seen`}})
	}
	if _, cerr := tx.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	took := journalHolds(t) - before
	t.Logf("index locks for %d flag changes in one transaction: %d", messages, took)
	if took != 1 {
		t.Errorf("one transaction over %d flag changes took %d index locks, want 1", messages, took)
	}

	msgs, gerr := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	for _, m := range msgs {
		var seen bool
		for _, fl := range m.Flags {
			if fl == `\Seen` {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("uid %d did not take the flag the transaction wrote", m.UID)
		}
	}
}
