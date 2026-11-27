package file

import (
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The last uid is 2^32-2, as the reference keeps uids below 2^32-1: every way
// of handing one out stops there, and none wraps to a uid the folder holds.
func TestTheUIDSpaceEndsBelow2To32(t *testing.T) {
	tests := []struct {
		name string
		next func(u *userIndex, id uint64) error
	}{
		{"AllocateUID", func(u *userIndex, id uint64) error { _, err := u.AllocateUID(id); return err }},
		{"AllocateAndAppend", func(u *userIndex, id uint64) error {
			return u.AllocateAndAppend(id, &mailbox.MessageMeta{Size: 10})
		}},
		{"a transaction", func(u *userIndex, id uint64) error {
			tx, err := u.Begin(id)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			tx.Append(&mailbox.MessageMeta{Size: 10})
			_, err = tx.Commit()
			return err
		}},
		{"a uid of 2^32-1", func(u *userIndex, id uint64) error {
			return u.AppendMessage(id, &mailbox.MessageMeta{UID: mailbox.MaxUID + 1, Size: 10})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := openIdx(t.TempDir(), testUser)
			defer u.Close() //nolint:errcheck
			f, err := u.OpenFolder("INBOX", 1, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := u.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Size: 10}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := u.AlignUIDSpace(f.ID, 0, mailbox.MaxUID); err != nil {
				t.Fatal(err)
			}
			last := &mailbox.MessageMeta{Size: 10}
			if err := u.AllocateAndAppend(f.ID, last); err != nil || last.UID != mailbox.MaxUID {
				t.Fatalf("the last uid: %d, %v; want %d", last.UID, err, uint32(mailbox.MaxUID))
			}
			for try := 0; try < 2; try++ { // a refusal moves no counter: the second one is refused too
				if err := tc.next(u, f.ID); !errors.Is(err, mailbox.ErrUIDSpaceFull) {
					t.Fatalf("past the last uid, try %d: %v, want the space full", try, err)
				}
				if err := u.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Size: 10}); !errors.Is(err, mailbox.ErrUIDSpaceFull) {
					t.Fatalf("a delivery after the refusal, try %d: %v, want the space full", try, err)
				}
			}
			msgs, err := u.GetMessages(f.ID, mailbox.SeqSet{})
			if err != nil || len(msgs) != 2 || msgs[0].UID != 1 {
				t.Fatalf("the folder holds %d records, %v; want uid 1 and the last alone", len(msgs), err)
			}
		})
	}
}
