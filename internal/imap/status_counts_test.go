package imap_test

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// STATUS answers MESSAGES, UNSEEN and DELETED from the folder's header, which
// must say what the flags say (#2661).
func TestStatusCountsComeFromTheHeader(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	for i := 0; i < 4; i++ {
		appendWithFlags(t, c, "INBOX", []byte(testMsg))
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	for _, st := range []struct {
		uid  imap.UID
		flag imap.Flag
	}{{1, imap.FlagSeen}, {2, imap.FlagSeen}, {3, imap.FlagDeleted}} {
		if err := c.Store(imap.UIDSetNum(st.uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{st.flag}}, nil).Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Unselect().Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.Status("INBOX", &imap.StatusOptions{NumMessages: true, NumUnseen: true, NumDeleted: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if *data.NumMessages != 4 || *data.NumUnseen != 2 || *data.NumDeleted != 1 {
		t.Errorf("STATUS MESSAGES %d UNSEEN %d DELETED %d, want 4, 2 and 1", *data.NumMessages, *data.NumUnseen, *data.NumDeleted)
	}
}
