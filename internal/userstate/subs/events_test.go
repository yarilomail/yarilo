package subs

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
)

// A subscription change is announced on the account's subscriptions; repeating
// one changes nothing and must not wake a push stream.
func TestSubscriptionWritesEmit(t *testing.T) {
	rec := &lockstest.Recorder{}
	s := New(t.TempDir(), "subscriptions", "u", locks.Owner("u", "test"), rec)
	steps := []struct {
		name  string
		write func() error
		emits bool
	}{
		{"add", func() error { return s.Add("Work") }, true},
		{"add again", func() error { return s.Add("Work") }, false},
		{"add own", func() error { _, err := s.AddOwn("Lists"); return err }, true},
		{"remove", func() error { return s.Remove("Work") }, true},
		{"remove again", func() error { return s.Remove("Work") }, false},
	}
	for _, st := range steps {
		before := rec.Count(locks.SubscriptionsKey("u"), locks.EventSubscriptions)
		if err := st.write(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if got := rec.Count(locks.SubscriptionsKey("u"), locks.EventSubscriptions) - before; got != map[bool]int{true: 1}[st.emits] {
			t.Errorf("%s emitted %d, want emits=%v", st.name, got, st.emits)
		}
	}
}
