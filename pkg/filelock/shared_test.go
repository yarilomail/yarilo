package filelock

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTakeModeSharedAndExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crypt.lock")
	a, err := TakeMode(path, MethodFlock, Shared, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b, err := TakeMode(path, MethodFlock, Shared, time.Second)
	if err != nil {
		t.Fatalf("a second shared hold waited: %v", err)
	}
	if _, err := TakeMode(path, MethodFlock, Exclusive, 20*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("exclusive beside shared holds: %v", err)
	}
	_ = a.Release()
	_ = b.Release()
	x, err := TakeMode(path, MethodFlock, Exclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TakeMode(path, MethodFlock, Shared, 20*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("shared beside an exclusive hold, same process: %v", err)
	}
	_ = x.Release()
}

// Releasing one hold leaves another of the same process in place: the reason
// flock and not fcntl.
func TestTakeModeReleaseDropsOnlyItsOwnHold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crypt.lock")
	a, _ := TakeMode(path, MethodFlock, Shared, time.Second)
	b, _ := TakeMode(path, MethodFlock, Shared, time.Second)
	_ = a.Release()
	if _, err := TakeMode(path, MethodFlock, Exclusive, 20*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("the remaining shared hold was dropped: %v", err)
	}
	_ = b.Release()
}

func TestTakeModeRefusesWithoutFlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crypt.lock")
	for _, m := range []Method{MethodFcntl, MethodDotlock} {
		if _, err := TakeMode(path, m, Shared, time.Second); !errors.Is(err, ErrNoSharedLock) {
			t.Errorf("%s: %v", m, err)
		}
	}
	prev := flockFn
	flockFn = func(int, int) error { return unix.ENOLCK }
	t.Cleanup(func() { flockFn = prev; forgetRefusals() })
	if _, err := TakeMode(path, MethodFlock, Shared, time.Second); !errors.Is(err, ErrNoSharedLock) {
		t.Fatalf("a volume refusing flock: %v", err)
	}
}

// A full volume still locks an existing file; only a missing one cannot be made.
func TestTakeModeOnAFullVolume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crypt.lock")
	undo := SetNoRoomForLockFiles()
	if _, err := TakeMode(path, MethodFlock, Shared, time.Second); !errors.Is(err, ErrNoSpaceForLock) {
		t.Fatalf("a missing file on a full volume: %v", err)
	}
	undo()
	h, err := TakeMode(path, MethodFlock, Shared, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Release()
	undo = SetNoRoomForLockFiles()
	defer undo()
	h, err = TakeMode(path, MethodFlock, Exclusive, time.Second)
	if err != nil {
		t.Fatalf("an existing file on a full volume: %v", err)
	}
	_ = h.Release()
}
