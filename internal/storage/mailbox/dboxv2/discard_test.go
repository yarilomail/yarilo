package dboxv2

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A discard whose temp is gone takes u.<uid> only when the GUID is the record's:
// another delivery may own that uid by the time the rollback runs.
func TestADiscardTakesOnlyItsOwnNamedBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ours   bool
		remain bool
	}{
		{"own body goes", true, false},
		{"another's body stays", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mb, home := newTestUser(t)
			named, _, guid := saveNamedGUID(t, mb, "INBOX", "Subject: x\r\n\r\nbody\r\n", 7, [16]byte{})
			m := &mailbox.MessageMeta{UID: 7, GUID: guid}
			if !tc.ours {
				m.GUID[0] ^= 0xff
			}
			if err := mb.(mailbox.SaveDiscarder).DiscardSaved("INBOX", ".temp.gone", m); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails", named))
			if remain := err == nil; remain != tc.remain {
				t.Fatalf("%s on disk after the discard: %v, want %v", named, remain, tc.remain)
			}
		})
	}
}
