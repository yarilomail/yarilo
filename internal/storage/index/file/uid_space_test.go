package file

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// One uid, one record: a second append under a uid the folder holds is refused,
// and the first record stays the only one (#2083).
func TestAUIDTheFolderHoldsIsRefused(t *testing.T) {
	u := openIdx(t.TempDir(), testUser)
	defer u.Close() //nolint:errcheck
	f, err := u.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 3, Size: 10}); err != nil {
		t.Fatal(err)
	}
	err = u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 3, Size: 20})
	if !errors.Is(err, mailbox.ErrUIDInUse) {
		t.Fatalf("a second record under uid 3 was not refused: %v", err)
	}
	msgs, err := u.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Size != 10 {
		t.Errorf("the folder holds %d records, the first of size %d; want the one of size 10", len(msgs), msgs[0].Size)
	}
}

// A reset to another UIDVALIDITY leaves nothing keyed by the old uids behind:
// the identity record names the new one, and the POP3 UIDLs are gone (#2083).
func TestAResetUIDSpaceLeavesNoOldUIDBehind(t *testing.T) {
	dir := t.TempDir()
	u := openIdx(dir, testUser)
	defer u.Close() //nolint:errcheck
	f, err := u.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}); err != nil {
		t.Fatal(err)
	}
	if err := u.SavePOP3UIDLs(f.ID, map[uint32]string{1: "old-uid-1"}); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(prev)

	current, reset, err := u.AlignUIDSpace(f.ID, 4242, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !reset || current != 4242 {
		t.Fatalf("align answered current=%d reset=%v, want 4242 and a reset", current, reset)
	}
	if v, ok, err := u.folders.UIDValidity("INBOX"); err != nil || !ok || v != 4242 {
		t.Errorf("the identity record names %d (known %v, err %v), want 4242", v, ok, err)
	}
	if _, err := os.Stat(filepath.Join(u.indexDir("INBOX"), "pop3.uidl")); !os.IsNotExist(err) {
		t.Errorf("the POP3 UIDLs keyed by the old uids are still there: %v", err)
	}
	if !strings.Contains(logged.String(), "UIDVALIDITY changed") {
		t.Errorf("the reset was not logged:\n%s", logged.String())
	}
	msgs, _ := u.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 0 {
		t.Errorf("%d records of the old generation survived the reset", len(msgs))
	}
}

// An empty folder taking a store's UID space records it as its identity, by
// either entry point: a lost index then reopens with it, not a new one.
func TestAnAdoptedUIDSpaceIsTheFoldersIdentity(t *testing.T) {
	cases := []struct {
		name  string
		adopt func(u *userIndex, id uint64) error
	}{
		{"align", func(u *userIndex, id uint64) error { _, _, err := u.AlignUIDSpace(id, 5151, 9); return err }},
		{"adopt", func(u *userIndex, id uint64) error { return u.AdoptUIDSpace(id, 5151, 9) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := openIdx(t.TempDir(), testUser)
			defer u.Close() //nolint:errcheck
			f, err := u.OpenFolder("INBOX", 1, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.adopt(u, f.ID); err != nil {
				t.Fatal(err)
			}
			if v, ok, err := u.folders.UIDValidity("INBOX"); err != nil || !ok || v != 5151 {
				t.Errorf("the identity record names %d (known %v, err %v), want 5151", v, ok, err)
			}
		})
	}
}
