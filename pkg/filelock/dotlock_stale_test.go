//go:build unix

package filelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A writer that crashed on a dotlock volume left its .lock behind, and every
// later writer waited for a holder that was gone until an operator removed the
// file. The reference takes such a lock over (file-dotlock.c:265-296); these
// rows hold both halves of that: abandoned is taken, alive is waited for.
func TestDotlockStaleIsTakenOverAndLiveIsNot(t *testing.T) {
	tests := []struct {
		name string
		// age is how long ago the abandoned lock and its file last changed.
		age     time.Duration
		owner   string
		want    bool
		wantErr error
	}{
		{name: "nothing changed for the timeout", age: time.Hour, owner: "999999 elsewhere\n", want: true},
		{name: "holder still writing", age: 0, owner: "999999 elsewhere\n", wantErr: ErrBusy},
		{name: "local holder is gone", age: 0, owner: fmt.Sprintf("%d %s\n", freePID(t), hostname(t)), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			SetStaleTimeout(time.Minute)
			t.Cleanup(func() { SetStaleTimeout(DefaultStaleTimeout) })

			dir := t.TempDir()
			path := filepath.Join(dir, "yarilo.index")
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatalf("seed the protected file: %v", err)
			}
			lockPath := path + ".lock"
			if err := os.WriteFile(lockPath, []byte(tc.owner), 0o600); err != nil {
				t.Fatalf("seed the lock: %v", err)
			}
			if tc.age > 0 {
				old := time.Now().Add(-tc.age)
				for _, p := range []string{path, lockPath} {
					if err := os.Chtimes(p, old, old); err != nil {
						t.Fatalf("age %s: %v", p, err)
					}
				}
			}

			h, err := Take(path, MethodDotlock, 50*time.Millisecond)
			if tc.want {
				if err != nil {
					t.Fatalf("Take = %v, want the abandoned lock taken over", err)
				}
				t.Cleanup(func() { _ = h.Release() })
				if _, perr := localPID(lockPath); !perr {
					t.Error("the new holder did not name itself in the lock file")
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Take = %v, want %v: a holder that is working must be waited for", err, tc.wantErr)
			}
			if b, rerr := os.ReadFile(lockPath); rerr != nil || string(b) != tc.owner {
				t.Errorf("the live holder's lock file was disturbed: %q, %v", b, rerr)
			}
		})
	}
}

// A hold keeps saying it is alive, so a long write is not taken from under it.
func TestHeldDotlockKeepsItselfFresh(t *testing.T) {
	SetStaleTimeout(90 * time.Millisecond)
	t.Cleanup(func() { SetStaleTimeout(DefaultStaleTimeout) })

	dir := t.TempDir()
	path := filepath.Join(dir, "yarilo.index")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h, err := Take(path, MethodDotlock, time.Second)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	defer func() { _ = h.Release() }()

	st, err := os.Lstat(path + ".lock")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	after, err := os.Lstat(path + ".lock")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !after.ModTime().After(st.ModTime()) {
		t.Errorf("the lock file has not changed in %s: a live holder reads as abandoned", 150*time.Millisecond)
	}
}

func hostname(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	return h
}

// freePID names a pid this host does not run. Taken from the top of the range
// so a live process cannot be hit.
func freePID(t *testing.T) int {
	t.Helper()
	for pid := 4194303; pid > 4194000; pid-- {
		if _, err := os.FindProcess(pid); err == nil && !processLives(pid) {
			return pid
		}
	}
	t.Skip("no free pid to stand in for a crashed holder")
	return 0
}
