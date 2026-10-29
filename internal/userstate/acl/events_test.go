package acl

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// Rights are a Mailbox property (myRights), so every ACL write is announced on
// the account's folder list; an update that declines to change anything is not.
func TestACLWritesEmit(t *testing.T) {
	rec := &lockstest.Recorder{}
	s := New(t.TempDir(), "", "maildir", "/", "", "u", locks.Owner("u", "test"), Policy{}, rec)
	bob := mailbox.ACL{{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"}}
	steps := []struct {
		name  string
		write func() error
		emits bool
	}{
		{"set", func() error { return s.Set("Work", bob) }, true},
		{"update declined", func() error {
			return s.Update("Work", func(mailbox.ACL) (mailbox.ACL, error) { return nil, nil })
		}, false},
		{"rename", func() error { return s.Rename("Work", "Projects") }, true},
		{"remove", func() error { return s.Remove("Projects") }, true},
	}
	for _, st := range steps {
		before := rec.Count(locks.MailboxListKey("u"), locks.EventACL)
		if err := st.write(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if got := rec.Count(locks.MailboxListKey("u"), locks.EventACL) - before; got != map[bool]int{true: 1}[st.emits] {
			t.Errorf("%s emitted %d, want emits=%v", st.name, got, st.emits)
		}
	}
}
