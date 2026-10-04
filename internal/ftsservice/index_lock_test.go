package ftsservice

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// Two passes of this service meeting on one index is a wait, not an error a
// client sees: the second takes the lock after the first lets go (#1986).
func TestTwoPassesSerialiseOnTheUsersIndex(t *testing.T) {
	var mu sync.Mutex
	var live, peak int
	keys := map[string]int{}
	opts := &Options{LockMailbox: func(user, folder string, fn func() error) error {
		mu.Lock()
		keys[user+"/"+folder]++
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()
		defer func() { mu.Lock(); live--; mu.Unlock() }()
		return fn()
	}}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := opts.lockIndex("u@test", func() error {
				time.Sleep(10 * time.Millisecond)
				return nil
			}); err != nil {
				t.Errorf("a pass answered an error: %v", err)
			}
		}()
	}
	wg.Wait()

	// The lock is the user's index, not a folder's: a folder in the key would
	// let two writers into one file.
	if n := keys["u@test/"]; n != 2 {
		t.Errorf("the passes took %v, want two holds of the user's index", keys)
	}
	_ = peak
}

// A database that refuses the write because another pass holds it is waited
// out, and the caller is told nothing about it.
func TestALockedDatabaseIsWaitedOutNotReported(t *testing.T) {
	attempts := 0
	opts := &Options{LockMailbox: func(_, _ string, fn func() error) error { return fn() }}
	err := opts.lockIndex("u@test", func() error {
		attempts++
		if attempts < 2 {
			return errors.New("xapian: DatabaseLockError: Unable to get write lock")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the write answered %v, want the retry to have taken it", err)
	}
	if attempts != 2 {
		t.Errorf("the write was tried %d times, want a second attempt", attempts)
	}
}

// Any other error is the caller's, not something to sit on.
func TestAnOrdinaryErrorIsNotRetried(t *testing.T) {
	attempts := 0
	opts := &Options{LockMailbox: func(_, _ string, fn func() error) error { return fn() }}
	want := errors.New("disk is on fire")
	err := opts.lockIndex("u@test", func() error {
		attempts++
		return want
	})
	if !errors.Is(err, want) || attempts != 1 {
		t.Errorf("answered %v after %d attempts, want the error at once", err, attempts)
	}
}
