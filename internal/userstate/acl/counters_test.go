package acl

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Rights are a Mailbox property (myRights), so every ACL write moves Mailbox;
// an update that declines to change anything does not.
func TestACLWritesMoveMailbox(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, "", "maildir", "/", "", "u", "o", Policy{}, nil, counters.New(dir, "u"))
	bob := mailbox.ACL{{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"}}
	steps := []struct {
		name  string
		write func() error
		moves bool
	}{
		{"set", func() error { return s.Set("Work", bob) }, true},
		{"update declined", func() error {
			return s.Update("Work", func(mailbox.ACL) (mailbox.ACL, error) { return nil, nil })
		}, false},
		{"rename", func() error { return s.Rename("Work", "Projects") }, true},
		{"remove", func() error { return s.Remove("Projects") }, true},
	}
	for _, st := range steps {
		before, _ := counters.Read(counters.Path(dir))
		if err := st.write(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		after, _ := counters.Read(counters.Path(dir))
		got := after.ChangedAfter(before.Highest)
		if moved := len(got) == 1 && got[0] == counters.Mailbox; moved != st.moves || (!st.moves && len(got) != 0) {
			t.Errorf("%s moved %v, want Mailbox=%v", st.name, got, st.moves)
		}
	}
}
