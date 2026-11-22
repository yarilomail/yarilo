package maildir

import (
	"os"
	"path/filepath"
	"testing"

	fileidx "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// raceFolder is three delivered messages seen by process a, and process b
// with the folder open and read.
type raceFolder struct {
	a, b   *userMailbox
	ia, ib mailbox.UserIndex
	fa, fb *mailbox.Folder
}

func newRaceFolder(t *testing.T) *raceFolder {
	t.Helper()
	home := testHome(t.TempDir(), "u@x.com")
	info := &mailbox.UserInfo{Username: "u@x.com", Home: home}
	r := &raceFolder{}
	r.a, r.ia = openWindowUser(t, home)
	if err := r.a.Init(); err != nil {
		t.Fatal(err)
	}
	if err := r.a.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	var err error
	if r.fa, err = r.ia.OpenFolder("INBOX", 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		deliverOne(t, r.a, r.ia, r.fa)
	}
	r.b = New().OpenUser(info).(*userMailbox)
	t.Cleanup(func() { r.b.Close() }) //nolint:errcheck
	r.ib = fileidx.New().OpenUser(info)
	t.Cleanup(func() { r.ib.Close() }) //nolint:errcheck
	if r.fb, err = r.ib.OpenFolder("INBOX", r.fa.UIDValidity); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ib.GetMessages(r.fb.ID, mailbox.SeqSet{}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *raceFolder) reconcileB(t *testing.T) mailbox.SyncStats {
	t.Helper()
	st, err := r.b.ReconcileIndex(mailboxbase.Open(r.b, r.ib), r.ib, r.fb)
	if err != nil {
		t.Fatalf("the reconcile stopped: %v", err)
	}
	return st
}

func uidsOf(t *testing.T, idx mailbox.UserIndex, folderID uint64) map[uint32]bool {
	t.Helper()
	msgs, err := idx.GetMessages(folderID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[uint32]bool, len(msgs))
	for _, m := range msgs {
		out[m.UID] = true
	}
	return out
}

// A record expunged in one process while its file and row are still there is
// an expunge another process's reconcile must not undo: once it waits, twice
// the file takes a new uid, and the expunged uid never returns (#2663).
func TestAnExpungedUIDDoesNotComeBack(t *testing.T) {
	r := newRaceFolder(t)
	if err := r.ia.ExpungeMessage(r.fa.ID, 2); err != nil {
		t.Fatal(err)
	}
	if st := r.reconcileB(t); st.Imported != 0 {
		t.Fatalf("the first pass imported %d; it waits for the expunge to finish", st.Imported)
	}
	if uidsOf(t, r.ib, r.fb.ID)[2] {
		t.Fatal("the first pass brought uid 2 back")
	}
	if st := r.reconcileB(t); st.Imported != 1 {
		t.Fatalf("the second pass imported %d, want the file under a new uid", st.Imported)
	}
	uids := uidsOf(t, r.ib, r.fb.ID)
	if uids[2] || len(uids) != 3 {
		t.Fatalf("after the second pass the folder holds %v; want 1, 3 and one new uid, never 2", uids)
	}
	if st := r.reconcileB(t); st.Imported != 0 || st.Expunged != 0 {
		t.Fatalf("a third pass changed %+v", st)
	}
}

// A record another process wrote after this one's view was published is
// seen by the reconcile, which imports nothing for it.
func TestAReconcileSeesARecordWrittenAfterItsView(t *testing.T) {
	r := newRaceFolder(t)
	deliverOne(t, r.a, r.ia, r.fa)
	if st := r.reconcileB(t); st.Imported != 0 || st.Expunged != 0 {
		t.Fatalf("the pass changed %+v for a record it should simply see", st)
	}
}

// An expunge removes the file before the record; a stop between leaves a
// record without a file, and the reconcile drops it: the message is under no
// uid, old or new.
func TestAStopBetweenTheFileAndTheRecordLosesTheMessage(t *testing.T) {
	r := newRaceFolder(t)
	box := mailboxbase.Open(r.a, r.ia)
	msgs, err := r.ia.GetMessages(r.fa.ID, mailbox.SeqSet{{From: 2, To: 2}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("uid 2: %d records, %v", len(msgs), err)
	}
	name, err := box.MessagePath("INBOX", msgs[0])
	if err != nil {
		t.Fatal(err)
	}
	var fileGone, recordThere bool
	defer mailboxbase.SetTestAfterBodyRemoved(func() {
		_, err := os.Stat(filepath.Join(r.a.folderPath("INBOX"), "cur", name))
		fileGone = os.IsNotExist(err)
		recordThere = uidsOf(t, r.ib, r.fb.ID)[2]
	})()
	if _, failed, err := box.ExpungeMarked(r.fa, "INBOX", msgs); err != nil || failed != 0 {
		t.Fatalf("expunge: %d failed, %v", failed, err)
	}
	if !fileGone || !recordThere {
		t.Fatalf("between the steps: file gone %v, record there %v; want the file first", fileGone, recordThere)
	}

	// The stop itself: the record stays, the file is gone.
	r2 := newRaceFolder(t)
	m2, err := r2.ia.GetMessages(r2.fa.ID, mailbox.SeqSet{{From: 2, To: 2}})
	if err != nil || len(m2) != 1 {
		t.Fatal(err)
	}
	n2, err := mailboxbase.Open(r2.a, r2.ia).MessagePath("INBOX", m2[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r2.a.folderPath("INBOX"), "cur", n2)); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		r2.reconcileB(t)
	}
	if uids := uidsOf(t, r2.ib, r2.fb.ID); uids[2] || len(uids) != 2 {
		t.Fatalf("after the reconcile the folder holds %v; want 1 and 3 alone", uids)
	}
}

// A body that cannot be removed keeps its record: the expunge reports it
// failed, and the message stays whole under its uid.
func TestABodyThatStaysKeepsItsRecord(t *testing.T) {
	r := newRaceFolder(t)
	box := mailboxbase.Open(r.a, r.ia)
	msgs, err := r.ia.GetMessages(r.fa.ID, mailbox.SeqSet{{From: 2, To: 2}})
	if err != nil || len(msgs) != 1 {
		t.Fatal(err)
	}
	cur := filepath.Join(r.a.folderPath("INBOX"), "cur")
	if err := os.Chmod(cur, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(cur, 0o700) //nolint:errcheck
	removed, failed, err := box.ExpungeMarked(r.fa, "INBOX", msgs)
	if err != nil || failed != 1 || len(removed) != 0 {
		t.Fatalf("removed %d, failed %d, err %v; want the one failure", len(removed), failed, err)
	}
	if !uidsOf(t, r.ia, r.fa.ID)[2] {
		t.Fatal("the record went though its body stayed")
	}
}
