package mailbox_test

import (
	"errors"
	"strings"
	"testing"

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
func TestFillSizesGivesRecordsTheSizeStorageHolds(t *testing.T) {
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
	filled, err := box.FillSizes(f)
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
