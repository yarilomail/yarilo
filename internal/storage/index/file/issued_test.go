package file

import (
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

func issuedCount(u *userIndex) int {
	u.issuedMu.Lock()
	defer u.issuedMu.Unlock()
	return len(u.issued)
}

// A uid below the folder's next one is let in once, only to the process that
// handed it out, and never after it is released or used (#2663).
func TestAUIDTheFolderGaveUpIsNotTakenAgain(t *testing.T) {
	tests := []struct {
		name string
		// run allocates in a and appends through a or b; it returns the
		// error of the append the rule judges.
		run func(t *testing.T, a, b *userHandle, folderID uint64) error
	}{
		{"released", func(t *testing.T, a, _ *userHandle, id uint64) error {
			uid := allocate(t, a, id)
			a.ReleaseUID(id, uid)
			return a.AppendMessage(id, &mailbox.MessageMeta{UID: uid, Size: 10})
		}},
		{"used once, expunged, offered again", func(t *testing.T, a, _ *userHandle, id uint64) error {
			uid := allocate(t, a, id)
			if err := a.AppendMessage(id, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
				t.Fatal(err)
			}
			if err := a.ExpungeMessage(id, uid); err != nil {
				t.Fatal(err)
			}
			return a.AppendMessage(id, &mailbox.MessageMeta{UID: uid, Size: 10})
		}},
		{"through a transaction", func(t *testing.T, a, _ *userHandle, id uint64) error {
			uid := allocate(t, a, id)
			a.ReleaseUID(id, uid)
			tx, err := a.Begin(id)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			tx.Append(&mailbox.MessageMeta{UID: uid, Size: 10})
			_, err = tx.Commit()
			return err
		}},
		{"handed out by another process", func(t *testing.T, a, b *userHandle, id uint64) error {
			uid := allocate(t, a, id)
			return b.AppendMessage(id, &mailbox.MessageMeta{UID: uid, Size: 10})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := testHome(t.TempDir(), testUser)
			info := &mailbox.UserInfo{Username: testUser, Home: home}
			a := New().OpenUser(info).(*userHandle)
			defer a.Close() //nolint:errcheck
			f, err := a.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Size: 10}); err != nil {
				t.Fatal(err)
			}
			b := New().OpenUser(info).(*userHandle)
			defer b.Close() //nolint:errcheck
			if _, err := b.OpenFolder("INBOX", 0); err != nil {
				t.Fatal(err)
			}
			if err := tc.run(t, a, b, f.ID); !errors.Is(err, mailbox.ErrUIDNotIssued) {
				t.Fatalf("the append was not refused as a uid given up: %v", err)
			}
			want := 0
			if tc.name == "handed out by another process" {
				want = 1 // a's, which b could not use
			}
			if n := issuedCount(a.ui); n != want {
				t.Fatalf("a holds %d issued uids, want %d", n, want)
			}
		})
	}
}

// An issued uid is let in, and leaves the list on its append.
func TestAnIssuedUIDIsLetInOnce(t *testing.T) {
	u := openIdx(t.TempDir(), testUser)
	defer u.Close() //nolint:errcheck
	f, err := u.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := u.AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := u.AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint32{second, first} {
		if err := u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
			t.Fatalf("issued uid %d refused: %v", uid, err)
		}
	}
	if n := issuedCount(u); n != 0 {
		t.Fatalf("%d issued uids outlived their appends", n)
	}
}

func allocate(t *testing.T, h *userHandle, folderID uint64) uint32 {
	t.Helper()
	uid, err := h.AllocateUID(folderID)
	if err != nil {
		t.Fatal(err)
	}
	return uid
}
