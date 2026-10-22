package specialuse

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
)

// A role is a Mailbox property, so setting or dropping one moves Mailbox.
func TestRoleWritesMoveMailbox(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, "u", "o", nil, nil, counters.New(dir, "u", "o", nil))
	steps := []struct {
		name  string
		write func() error
		moves bool
	}{
		{"set", func() error { return s.Set("Archive", `\Archive`) }, true},
		{"set again", func() error { return s.Set("Archive", `\Archive`) }, false},
		{"delete", func() error { return s.Delete("Archive") }, true},
		{"delete again", func() error { return s.Delete("Archive") }, false},
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
