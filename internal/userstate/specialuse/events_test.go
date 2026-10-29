package specialuse

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
)

// A role is a Mailbox property, so setting or dropping one is announced on the
// account's folder list.
func TestRoleWritesEmit(t *testing.T) {
	rec := &lockstest.Recorder{}
	s := New(t.TempDir(), "u", locks.Owner("u", "test"), rec, nil)
	steps := []struct {
		name  string
		write func() error
		emits bool
	}{
		{"set", func() error { return s.Set("Archive", `\Archive`) }, true},
		{"set again", func() error { return s.Set("Archive", `\Archive`) }, false},
		{"delete", func() error { return s.Delete("Archive") }, true},
		{"delete again", func() error { return s.Delete("Archive") }, false},
	}
	for _, st := range steps {
		before := rec.Count(locks.MailboxListKey("u"), locks.EventRole)
		if err := st.write(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if got := rec.Count(locks.MailboxListKey("u"), locks.EventRole) - before; got != map[bool]int{true: 1}[st.emits] {
			t.Errorf("%s emitted %d, want emits=%v", st.name, got, st.emits)
		}
	}
}
