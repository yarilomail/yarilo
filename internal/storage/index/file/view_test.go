package file

import (
	"os"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Fifty readers under one writer: a reader takes no lock, and what it sees is
// always a whole transaction — never half of one (#1809).
func TestManyReadersUnderAWriterTakeNoLockAndSeeWholeTransactions(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "iris@example.com")
	ui := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "iris@example.com", Home: home,
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}

	const (
		readers = 50
		batches = 20
		perTx   = 5
	)
	before := sharedAcquisitions(t)

	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		uid := uint32(1)
		for b := 0; b < batches; b++ {
			tx, terr := ui.Begin(f.ID)
			if terr != nil {
				t.Errorf("begin: %v", terr)
				return
			}
			for i := 0; i < perTx; i++ {
				tx.Append(&mailbox.MessageMeta{UID: uid, Size: 10})
				uid++
			}
			if _, cerr := tx.Commit(); cerr != nil {
				t.Errorf("commit: %v", cerr)
				return
			}
		}
		close(stop)
	}()

	var readersWG sync.WaitGroup
	bad := make(chan int, readers)
	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				msgs, gerr := ui.GetMessages(f.ID, mailbox.SeqSet{})
				if gerr != nil {
					t.Errorf("read: %v", gerr)
					return
				}
				// Every commit adds exactly perTx records, so a count that is
				// not a multiple of it is half a transaction.
				if len(msgs)%perTx != 0 {
					bad <- len(msgs)
					return
				}
			}
		}()
	}
	writer.Wait()
	readersWG.Wait()
	close(bad)

	if n, ok := <-bad; ok {
		t.Errorf("a reader saw %d records, which is not a whole number of transactions", n)
	}
	if got := sharedAcquisitions(t) - before; got != 0 {
		t.Errorf("fifty readers took %v shared acquisitions, want none", got)
	}
}

// A reader whose base is replaced under it keeps reading a whole set: the old
// image stands until the reader is done with it.
func TestAReaderSeesAWholeSetAcrossABaseRewrite(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "iris@example.com")
	ui := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "iris@example.com", Home: home,
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	for i := 1; i <= 20; i++ {
		if aerr := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i), Size: 10}); aerr != nil {
			t.Fatal(aerr)
		}
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			msgs, gerr := ui.GetMessages(f.ID, mailbox.SeqSet{})
			if gerr != nil {
				t.Errorf("read across the rewrite: %v", gerr)
				return
			}
			if len(msgs) < 20 {
				t.Errorf("a reader saw %d records across a base rewrite, want at least 20", len(msgs))
				return
			}
		}
	}()

	for i := 0; i < 10; i++ {
		if oerr := ui.OptimizeIndex(f.ID); oerr != nil {
			t.Fatalf("optimize: %v", oerr)
		}
	}
	close(done)
	wg.Wait()
}

// A transaction whose boundary promises more than the file holds is not a
// state anything may read. RED on purpose: the rule is not implemented -- the
// replay applies records as they decode and the boundary only bookkeeps the
// offset, so a group cut short is served. The first attempt at enforcing it
// (skip a group whose end exceeds the size the log was opened at) cost live
// appends their records, because that size is a snapshot of a file a writer is
// still extending. Staging a group and committing it at its boundary is the
// shape that works, and it is not written yet (#1831).
func TestATransactionPastTheLastBoundaryIsNotRead(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "iris@example.com")
	ui := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "iris@example.com", Home: home,
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	commit := func(from, to uint32) {
		t.Helper()
		tx, terr := ui.Begin(f.ID)
		if terr != nil {
			t.Fatal(terr)
		}
		for uid := from; uid <= to; uid++ {
			tx.Append(&mailbox.MessageMeta{UID: uid, Size: 10})
		}
		if _, cerr := tx.Commit(); cerr != nil {
			t.Fatal(cerr)
		}
	}
	commit(1, 5)
	settled, gerr := ui.GetMessages(f.ID, mailbox.SeqSet{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if len(settled) != 5 {
		t.Fatalf("the first transaction reads as %d records, want 5", len(settled))
	}

	fs := ui.open[f.ID]
	logPath := fs.indexPath + ".log"
	sizeBefore := func() int64 {
		st, serr := os.Stat(logPath)
		if serr != nil {
			t.Fatal(serr)
		}
		return st.Size()
	}()

	// A second transaction, then the file cut short of what its boundary
	// promises: the crash between the write reaching the page cache and the
	// whole of it reaching disk.
	commit(6, 10)
	if full, ferr := ui.GetMessages(f.ID, mailbox.SeqSet{}); ferr != nil || len(full) != 10 {
		t.Fatalf("before the cut the folder reads as %d records (err %v), want 10 — the trap would prove nothing", len(full), ferr)
	}
	// Four bytes short of the whole: the boundary and all but the last of its
	// sub-records are on disk, and the boundary promises what is missing.
	after, serr := os.Stat(logPath)
	if serr != nil {
		t.Fatal(serr)
	}
	cut := after.Size() - 4
	if cut <= sizeBefore {
		t.Fatalf("the second transaction wrote %d bytes; too few to cut inside", after.Size()-sizeBefore)
	}
	if terr := os.Truncate(logPath, cut); terr != nil {
		t.Fatal(terr)
	}

	got, aerr := ui.GetMessages(f.ID, mailbox.SeqSet{})
	if aerr != nil {
		t.Fatalf("read over an incomplete transaction: %v", aerr)
	}
	if len(got) != 5 {
		t.Errorf("a transaction whose boundary is not met reads as %d records, want the 5 that were committed", len(got))
	}
}
