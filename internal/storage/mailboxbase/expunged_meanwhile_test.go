package mailboxbase_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// boxOn is one account on a real store of the driver, through both halves.
func boxOn(t *testing.T, driver string) (*mailboxbase.Box, *mailbox.Folder) {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir(), Driver: driver}
	var store mailbox.UserMailbox
	switch driver {
	case "maildir":
		store = maildir.New().OpenUser(info)
	case "sdbox":
		store = dboxv2.New().OpenUser(info)
	}
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	box := mailboxbase.Open(store, idx)
	f, err := box.Folder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	return box, f
}

// deliverOne records one message and returns its record as a reader holds it.
func deliverOne(t *testing.T, box *mailboxbase.Box, f *mailbox.Folder) *mailbox.MessageMeta {
	t.Helper()
	const body = "From: a@b\r\nSubject: s\r\n\r\nbody\r\n"
	uid, err := box.Index().AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, vsize, guid, err := box.Store().Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := box.RecordDelivered(f, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	msgs, err := box.Messages(f.ID, mailbox.SeqSet{{From: uid, To: uid}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("record: %v %v", msgs, err)
	}
	return msgs[0]
}

type reader func(*mailboxbase.Box, *mailbox.MessageMeta) error

var readers = map[string]reader{
	"OpenMessage": func(b *mailboxbase.Box, m *mailbox.MessageMeta) error {
		rc, err := b.OpenMessage("INBOX", m)
		if err == nil {
			_, _ = io.Copy(io.Discard, rc)
			_ = rc.Close()
		}
		return err
	},
	"RawMessage": func(b *mailboxbase.Box, m *mailbox.MessageMeta) error {
		raw, err := b.RawMessage("INBOX", m)
		if err == nil {
			_ = raw.Close()
		}
		return err
	},
}

// A message another session expunged between the list and the read reads as
// ErrExpunged, on maildir and sdbox, through both reads, with nothing in the
// error that marks the folder corrupt.
func TestABodyGoneWithItsRecordIsExpunged(t *testing.T) {
	for _, driver := range []string{"maildir", "sdbox"} {
		for name, read := range readers {
			t.Run(driver+"/"+name, func(t *testing.T) {
				box, f := boxOn(t, driver)
				m := deliverOne(t, box, f)
				if err := box.RemoveMessage("INBOX", m); err != nil {
					t.Fatal(err)
				}
				if err := box.Index().ExpungeMessage(f.ID, m.UID); err != nil {
					t.Fatal(err)
				}
				err := read(box, m)
				if !errors.Is(err, mailbox.ErrExpunged) {
					t.Fatalf("read = %v, want ErrExpunged", err)
				}
				if errors.Is(err, mailbox.ErrCorruptStorage) {
					t.Errorf("an expunge carries ErrCorruptStorage, which marks the folder: %v", err)
				}
			})
		}
	}
}

// A body gone while the index still holds its record is the store's error,
// not an expunge: that is damage, and the heal exists for it.
func TestABodyGoneWithItsRecordKeptIsNoExpunge(t *testing.T) {
	for _, driver := range []string{"maildir", "sdbox"} {
		for name, read := range readers {
			t.Run(driver+"/"+name, func(t *testing.T) {
				box, f := boxOn(t, driver)
				m := deliverOne(t, box, f)
				if err := box.RemoveMessage("INBOX", m); err != nil {
					t.Fatal(err)
				}
				err := read(box, m)
				if err == nil || errors.Is(err, mailbox.ErrExpunged) {
					t.Fatalf("read = %v, want the store's error", err)
				}
			})
		}
	}
}

// The index is asked only after a read found no body, once, and never on a
// read that worked.
func TestTheIndexIsAskedOncePerVanishedBody(t *testing.T) {
	for _, driver := range []string{"maildir", "sdbox"} {
		t.Run(driver, func(t *testing.T) {
			asks := 0
			defer mailboxbase.SetBeforeExpungeCheck(func() { asks++ })()
			box, f := boxOn(t, driver)
			m := deliverOne(t, box, f)
			for _, read := range readers {
				if err := read(box, m); err != nil {
					t.Fatal(err)
				}
			}
			if asks != 0 {
				t.Errorf("reads that worked asked the index %d times", asks)
			}
			if err := box.RemoveMessage("INBOX", m); err != nil {
				t.Fatal(err)
			}
			_ = readers["OpenMessage"](box, m)
			if asks != 1 {
				t.Errorf("one vanished body asked the index %d times, want 1", asks)
			}
		})
	}
}
