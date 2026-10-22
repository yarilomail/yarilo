package subs

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
)

// A subscription is a Mailbox property, so a change moves Mailbox; repeating
// one changes nothing and must not wake a push stream.
func TestSubscriptionWritesMoveMailbox(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, "subscriptions", "u", "o", nil, counters.New(dir, "u"))
	steps := []struct {
		name  string
		write func() error
		moves bool
	}{
		{"add", func() error { return s.Add("Work") }, true},
		{"add again", func() error { return s.Add("Work") }, false},
		{"add own", func() error { _, err := s.AddOwn("Lists"); return err }, true},
		{"remove", func() error { return s.Remove("Work") }, true},
		{"remove again", func() error { return s.Remove("Work") }, false},
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
