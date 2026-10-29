package mailboxbase_test

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// baseOnly shows a driver through mailbox.UserMailbox alone: none of the
// optional surfaces a driver may add.
type baseOnly struct{ mailbox.UserMailbox }

// A driver with only the base surface still works through the box: a held
// remove is a plain Remove, sizes are the record's, and there is nothing to heal.
func TestADriverWithOnlyTheBaseSurfaceWorks(t *testing.T) {
	info := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir(), Driver: "sdbox"}
	inner := dboxv2.New().OpenUser(info)
	if err := inner.Init(); err != nil {
		t.Fatal(err)
	}
	store := baseOnly{inner}
	idx := file.New().OpenUser(info)
	box := mailboxbase.Open(store, idx)
	defer box.Close()
	f, err := box.Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	name, _, _, err := store.Save("INBOX", strings.NewReader("Subject: b\r\n\r\nbase\r\n"), 0, 20, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.RemoveHeld("INBOX", name); err != nil {
		t.Errorf("held remove through a base driver: %v", err)
	}
	m := &mailbox.MessageMeta{UID: 1, Size: 20}
	if size, vsize, err := mailboxbase.MessageSize(store, "INBOX", m); err != nil || size != 20 || vsize != 0 {
		t.Errorf("sizes %d/%d, %v; want the record's own", size, vsize, err)
	}
	if got := mailboxbase.RFC822SizeOf(store, "INBOX", m); got != 20 {
		t.Errorf("RFC822.SIZE %d, want the physical 20 with no virtual size", got)
	}
	if n, err := mailboxbase.FillSizelessRecords(idx, store, f); err != nil || n != 0 {
		t.Errorf("filled %d records, %v; a base driver has no sizes to give", n, err)
	}
	if gone, err := box.HealCorrupt(f); err != nil || gone != nil {
		t.Errorf("heal: %v, %v", gone, err)
	}
}
