package ftsservice

import (
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// splitIndex reports how long the lock was held against how long the whole
// compaction took.
type splitIndex struct {
	stubUserIndex
	mergeFor  time.Duration
	switchFor time.Duration
	held      time.Duration
}

func (s *splitIndex) Mailboxes() []fts.MailboxRef { return []fts.MailboxRef{{}} }

func (s *splitIndex) OptimizeUnderLock(_ fts.MailboxRef, withLock func(func() error) error) error {
	time.Sleep(s.mergeFor) // the merge, outside the lock
	return withLock(func() error {
		time.Sleep(s.switchFor)
		return nil
	})
}

// A compaction takes the lock only for the switch: holding it for the merge
// shuts deliveries out for as long as the merge runs (#1986).
func TestACompactionHoldsTheLockOnlyForTheSwitch(t *testing.T) {
	idx := &splitIndex{mergeFor: 120 * time.Millisecond, switchFor: 5 * time.Millisecond}
	var mu sync.Mutex
	opts := &Options{LockMailbox: func(_, _ string, fn func() error) error {
		t0 := time.Now()
		err := fn()
		mu.Lock()
		idx.held += time.Since(t0)
		mu.Unlock()
		return err
	}}
	s := &Service{opts: *opts}

	t0 := time.Now()
	if err := s.optimize(&userHandle{ui: idx}, "u@test", fts.MailboxRef{}); err != nil {
		t.Fatal(err)
	}
	total := time.Since(t0)

	mu.Lock()
	held := idx.held
	mu.Unlock()
	if held >= idx.mergeFor {
		t.Errorf("the lock was held %v of the %v the compaction took: the merge ran under it",
			held, total)
	}
}
