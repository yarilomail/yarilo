package file

import (
	"encoding/binary"
	"os"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
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

// A half-written record after the last boundary is not a state anything may
// read: the reader answers from the boundary, not from the torn tail (#1831).
func TestATornTailIsNotRead(t *testing.T) {
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
	tx, terr := ui.Begin(f.ID)
	if terr != nil {
		t.Fatal(terr)
	}
	for i := 1; i <= 5; i++ {
		tx.Append(&mailbox.MessageMeta{UID: uint32(i), Size: 10})
	}
	if _, cerr := tx.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	whole, gerr := ui.GetMessages(f.ID, mailbox.SeqSet{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if len(whole) != 5 {
		t.Fatalf("the committed transaction reads as %d records, want 5", len(whole))
	}

	// A record header claiming more bytes than the file holds: what a crash
	// between the write and its completion leaves behind.
	fs := ui.open[f.ID]
	lf, oerr := os.OpenFile(fs.indexPath+".log", os.O_WRONLY|os.O_APPEND, 0o600)
	if oerr != nil {
		t.Fatal(oerr)
	}
	torn := make([]byte, 8)
	binary.LittleEndian.PutUint32(torn[0:], 4096)
	binary.LittleEndian.PutUint32(torn[4:], uint32(mailindex.TxTypeAppend))
	if _, werr := lf.Write(append(torn, 0x01, 0x02, 0x03)); werr != nil {
		t.Fatal(werr)
	}
	_ = lf.Close()

	after, aerr := ui.GetMessages(f.ID, mailbox.SeqSet{})
	if aerr != nil {
		t.Fatalf("read over a torn tail: %v", aerr)
	}
	if len(after) != len(whole) {
		t.Errorf("a torn tail changed the answer from %d records to %d", len(whole), len(after))
	}
}
