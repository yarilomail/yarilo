package mailbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// openBox is one account through both halves at once.
func openBox(t *testing.T, user string) (*mailbox.Box, *mailbox.Folder) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: user, Home: home, Driver: "maildir"}
	store := maildir.New().OpenUser(info)
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	box := mailbox.Open(store, idx)
	f, err := box.Folder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	return box, f
}

// A delivery is recorded name first: the record a client sees names a file
// that is already there (#1745).
func TestRecordDeliveredNamesTheMessageBeforeRecordingIt(t *testing.T) {
	box, f := openBox(t, "u1@example.com")
	const body = "From: a@b\r\nSubject: delivered\r\n\r\nbody\r\n"
	uid, err := box.Index().AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, vsize, guid, err := box.Store().Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := box.RecordDelivered(f, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}

	msgs, err := box.Index().GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the folder holds %d records, want 1", len(msgs))
	}
	name, err := mailbox.MessagePath(box.Store(), "INBOX", msgs[0])
	if err != nil || name == "" {
		t.Fatalf("the recorded message names no file: %q %v", name, err)
	}
}

// refusingNamer is a store whose driver will not settle a name.
type refusingNamer struct {
	mailbox.UserMailbox
}

func (r *refusingNamer) AssignUID(string, string, uint32) (string, error) {
	return "", errRefusedName
}

var errRefusedName = errors.New("the name was refused")

// A name that cannot be settled leaves no record behind: half a delivery is a
// message a client sees and cannot read (#1745).
func TestRecordDeliveredLeavesNoRecordWhenTheNameFails(t *testing.T) {
	plain, f := openBox(t, "u2@example.com")
	box := mailbox.Open(&refusingNamer{UserMailbox: plain.Store()}, plain.Index())
	m := &mailbox.MessageMeta{UID: 7, Size: 10, VSize: 10}
	err := box.RecordDelivered(f, "INBOX", "1700000000.M1P1.host:2,", m)
	if err == nil {
		t.Fatal("naming a message that was never saved was accepted")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("the refusal is %q and does not say which half failed", err)
	}
	msgs, gerr := box.Index().GetMessages(f.ID, mailbox.SeqSet{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if len(msgs) != 0 {
		t.Errorf("the folder holds %d records after a delivery that could not be named", len(msgs))
	}
}

// The box fills what its records lack, so a sum over the folder is taken on
// mail and not on zeros (#1728).
func TestFillSizelessGivesRecordsTheSizeStorageHolds(t *testing.T) {
	box, f := openBox(t, "u3@example.com")
	const body = "From: a@b\r\nSubject: sized\r\n\r\nbody\r\n"
	uid, err := box.Index().AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, _, guid, err := box.Store().Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	// No sizes on the record: the state a folder recovered from storage is in.
	m := &mailbox.MessageMeta{UID: uid, GUID: guid}
	if err := box.RecordDelivered(f, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	filled, err := box.FillSizeless(f)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 1 {
		t.Fatalf("the pass filled %d records, want 1", filled)
	}
	msgs, err := box.Index().GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if got := mailbox.RFC822SizeOf(box.Store(), "INBOX", msgs[0]); got != uint32(len(body)) {
		t.Errorf("the record answers size %d, the body is %d", got, len(body))
	}
}

// A batch takes the folder once, not once per message: the count is the number
// of round trips the service sees for one POP3 UPDATE (#1715).
func TestExpungeMarkedTakesTheFolderOnce(t *testing.T) {
	box, f := openBox(t, "u4@example.com")
	lk := &countingLocker{held: map[string]locks.HoldMode{}}
	batched := mailbox.Open(box.Store(), box.Index(), mailbox.WithLocker(lk, "test/0/u4@example.com/s1"))

	msgs := make([]*mailbox.MessageMeta, 0, 3)
	for i := 0; i < 3; i++ {
		body := "From: a@b\r\nSubject: m\r\n\r\nbody\r\n"
		uid, err := box.Index().AllocateUID(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		saved, vsize, guid, serr := box.Store().Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
		if err := box.RecordDelivered(f, "INBOX", saved, m); err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}

	lk.locks = 0
	removed, failed := batched.ExpungeMarked(f, "INBOX", msgs)
	if failed != 0 || len(removed) != 3 {
		t.Fatalf("the batch removed %v and failed %d, want three removed", removed, failed)
	}
	if lk.locks != 1 {
		t.Errorf("the batch took the folder %d times for 3 messages, want 1", lk.locks)
	}
	left, err := box.Index().GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("the folder still holds %d records after the batch", len(left))
	}
}

// Without a locker the batch still removes everything: a dev run and a test
// have no lock service, and the promise is the messages, not the hold.
func TestExpungeMarkedWorksWithNoLocker(t *testing.T) {
	box, f := openBox(t, "u5@example.com")
	const body = "From: a@b\r\nSubject: m\r\n\r\nbody\r\n"
	uid, err := box.Index().AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, vsize, guid, serr := box.Store().Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
	if serr != nil {
		t.Fatal(serr)
	}
	m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := box.RecordDelivered(f, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	removed, failed := box.ExpungeMarked(f, "INBOX", []*mailbox.MessageMeta{m})
	if failed != 0 || len(removed) != 1 {
		t.Fatalf("removed %v, failed %d", removed, failed)
	}
}

// countingLocker counts the times the folder key was taken.
type countingLocker struct {
	locks.Locker
	locks int
	held  map[string]locks.HoldMode
}

func (l *countingLocker) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	l.locks++
	l.held[resource] = locks.HoldExclusive
	return locks.Lock{ID: resource, Resource: resource, Owner: owner}, nil
}

func (l *countingLocker) Unlock(_ context.Context, id string) error {
	delete(l.held, id)
	return nil
}

func (l *countingLocker) HoldsResource(resource string) (locks.HoldMode, bool) {
	mode, ok := l.held[resource]
	return mode, ok
}

// A stop between the two steps leaves a file with no record, which the next
// reconcile re-files, and never a record naming a file that is gone (#1690).
func TestTheRecordGoesBeforeTheBody(t *testing.T) {
	box, f := openBox(t, "u6@example.com")
	const body = "From: a@b\r\nSubject: order\r\n\r\nbody\r\n"
	uid, err := box.Index().AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, vsize, guid, serr := box.Store().Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
	if serr != nil {
		t.Fatal(serr)
	}
	m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := box.RecordDelivered(f, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	name, err := mailbox.MessagePath(box.Store(), "INBOX", m)
	if err != nil {
		t.Fatal(err)
	}

	// What the folder holds at the moment between the two steps.
	var recordsThen int
	var bodyThen bool
	disarm := mailbox.SetTestAfterRecordExpunged(func() {
		msgs, gerr := box.Index().GetMessages(f.ID, mailbox.SeqSet{})
		if gerr != nil {
			t.Error(gerr)
		}
		recordsThen = len(msgs)
		rc, ferr := box.Store().Fetch("INBOX", name, false)
		if ferr == nil {
			bodyThen = true
			rc.Close() //nolint:errcheck
		}
	})
	removed, failed := box.ExpungeMarked(f, "INBOX", []*mailbox.MessageMeta{m})
	disarm()
	if failed != 0 || len(removed) != 1 {
		t.Fatalf("removed %v, failed %d", removed, failed)
	}
	if recordsThen != 0 {
		t.Errorf("at the stop the folder still held %d records: the body went first", recordsThen)
	}
	if !bodyThen {
		t.Error("at the stop the body was already gone: the body went first")
	}
}

// A record resolving to nothing is reported, never answered as an empty
// message: a client told "zero octets" acts on it (#1715).
func TestAnUnresolvableRecordIsReportedNotEmptied(t *testing.T) {
	box, f := openBox(t, "u7@example.com")
	// A record with no name at all: the state a folder recovered from storage
	// without its list is in.
	m := &mailbox.MessageMeta{UID: 9, Size: 40}
	if err := box.Index().AppendMessage(f.ID, m); err != nil {
		t.Fatal(err)
	}
	if box.Readable(m) && func() bool {
		name, err := box.MessagePath("INBOX", m)
		return err == nil && name != ""
	}() {
		t.Fatal("a record naming no file was accepted as readable")
	}
	if _, err := box.OpenMessage("INBOX", m); err == nil {
		t.Error("opening a record that names no file was accepted")
	}
	if got := box.RFC822Size("INBOX", m); got != 40 {
		t.Errorf("the size answered is %d; the record's own is 40 and storage cannot be asked", got)
	}
}

// The size a client is told is the size of the body it gets, including for a
// record that carries none — what a recovered folder holds (#1726, #1727).
func TestTheSizeToldIsTheSizeOfTheBody(t *testing.T) {
	box, f := openBox(t, "u8@example.com")
	const body = "From: a@b\r\nSubject: sized\r\n\r\nbody\r\n"
	uid, err := box.Index().AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, _, guid, serr := box.Store().Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
	if serr != nil {
		t.Fatal(serr)
	}
	// No sizes on the record, as a recovered one has.
	m := &mailbox.MessageMeta{UID: uid, GUID: guid}
	if err := box.RecordDelivered(f, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	read, err := box.Index().GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil || len(read) != 1 {
		t.Fatalf("records: %d %v", len(read), err)
	}
	if got := box.RFC822Size("INBOX", read[0]); got != uint32(len(body)) {
		t.Errorf("the client is told %d octets, the body is %d", got, len(body))
	}
	box.FillResponseSizes("INBOX", read)
	if read[0].VSize != uint32(len(body)) {
		t.Errorf("after the stamp the record carries %d, the body is %d", read[0].VSize, len(body))
	}
}

// A flag change reaches storage: one kept in the index alone leaves the store
// describing the message as it arrived (#1601).
func TestWriteFlagsSettlesInStorage(t *testing.T) {
	box, f := openBox(t, "u9@example.com")
	const body = "From: a@b\r\nSubject: flags\r\n\r\nbody\r\n"
	uid, err := box.Index().AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, vsize, guid, serr := box.Store().Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
	if serr != nil {
		t.Fatal(serr)
	}
	m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := box.RecordDelivered(f, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	name, err := box.MessagePath("INBOX", m)
	if err != nil {
		t.Fatal(err)
	}

	results := box.WriteFlags(f, "INBOX", []mailbox.FlagWrite{
		{UID: uid, Filename: name, Flags: []string{`\Seen`}},
	})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("the write answered %+v", results)
	}
	// The name carries it, which is where a maildir keeps a flag.
	if !strings.Contains(results[0].Filename, "S") {
		t.Errorf("the message is called %q and \\Seen was set", results[0].Filename)
	}
	// And a reader of storage alone sees it.
	msgs, err := box.Store().List("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, sm := range msgs {
		for _, fl := range sm.Flags {
			if fl == `\Seen` {
				seen = true
			}
		}
	}
	if !seen {
		t.Error("storage does not hold the flag: the change stayed in the index")
	}
}

// A copy settles its name in the destination before its record: the same rule
// as a delivery, on the destination's own halves (#1745).
func TestCopySettlesTheNameBeforeTheRecord(t *testing.T) {
	box, f := openBox(t, "u10@example.com")
	if err := box.Store().Create("Archive"); err != nil {
		t.Fatal(err)
	}
	dst, err := box.Folder("Archive", 1)
	if err != nil {
		t.Fatal(err)
	}
	const body = "From: a@b\r\nSubject: copied\r\n\r\nbody\r\n"
	saved, vsize, guid, serr := box.Store().Save("Archive", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if serr != nil {
		t.Fatal(serr)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := box.Copy(dst, "Archive", saved, m); err != nil {
		t.Fatal(err)
	}
	msgs, err := box.Index().GetMessages(dst.ID, mailbox.SeqSet{})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("the destination holds %d records: %v", len(msgs), err)
	}
	if name, perr := box.MessagePath("Archive", msgs[0]); perr != nil || name == "" {
		t.Errorf("the copied record names no file: %q %v", name, perr)
	}
	_ = f
}
