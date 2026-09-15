package filelock

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Every transport serialises writers on one file: that is the whole contract.
func TestATransportAdmitsOneWriterAtATime(t *testing.T) {
	for _, method := range []Method{MethodFcntl, MethodFlock, MethodDotlock} {
		t.Run(string(method), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "uidlist")
			var inside, most int
			var mu sync.Mutex
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					h, err := Take(path, method, 10*time.Second)
					if err != nil {
						t.Errorf("take: %v", err)
						return
					}
					mu.Lock()
					inside++
					if inside > most {
						most = inside
					}
					mu.Unlock()
					time.Sleep(time.Millisecond)
					mu.Lock()
					inside--
					mu.Unlock()
					if rerr := h.Release(); rerr != nil {
						t.Errorf("release: %v", rerr)
					}
				}()
			}
			wg.Wait()
			if most != 1 {
				t.Errorf("%d writers were inside at once, want 1", most)
			}
		})
	}
}

// A taker that cannot have it gives up on its own deadline rather than waiting
// on a wedged mount for ever.
func TestATakerGivesUpOnItsDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uidlist")
	held, err := Take(path, MethodDotlock, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	started := time.Now()
	if _, err := Take(path, MethodDotlock, 100*time.Millisecond); err == nil {
		t.Fatal("a second taker got a lock that is held")
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("the second taker waited %v past its deadline", took)
	}
}

// A released lock leaves nothing behind that would block the next taker.
func TestAReleasedDotlockLeavesNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uidlist")
	h, err := Take(path, MethodDotlock, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if rerr := h.Release(); rerr != nil {
		t.Fatal(rerr)
	}
	if _, serr := os.Stat(path + ".lock"); !os.IsNotExist(serr) {
		t.Error("the dotlock outlived its holder")
	}
}

func TestParseNamesTheTransport(t *testing.T) {
	for in, want := range map[string]Method{"": MethodFlock, "fcntl": MethodFcntl, "flock": MethodFlock, "dotlock": MethodDotlock} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Parse("posix"); err == nil {
		t.Error("an unknown transport was accepted")
	}
}

// A reader opening and closing the same file must not drop the writer's lock.
// A POSIX record lock does exactly that -- it belongs to the process and goes
// on any close -- which is why flock is the default (#1840).
func TestAnUnrelatedCloseDoesNotDropTheLock(t *testing.T) {
	for _, tc := range []struct {
		method Method
		keeps  bool
	}{
		{MethodFlock, true},
		{MethodFcntl, false},
	} {
		t.Run(string(tc.method), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "uidlist")
			held, err := takeShared(path, tc.method, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Release() }()

			// What a reader does: open the file, read nothing, close it.
			rf, oerr := os.Open(path)
			if oerr != nil {
				t.Fatal(oerr)
			}
			_ = rf.Close()

			// takeShared skips the in-process mutex, so this asks the kernel
			// the same question another process would.
			second, serr := takeShared(path, tc.method, 100*time.Millisecond)
			if second != nil {
				_ = second.Release()
			}
			stillHeld := serr != nil
			if stillHeld != tc.keeps {
				if tc.keeps {
					t.Errorf("%s lost the lock to a reader's close", tc.method)
				} else {
					t.Errorf("%s kept the lock through a close; the default could be either", tc.method)
				}
			}
		})
	}
}

// The volume is asked at startup, not trusted: a mount that admits two writers
// is found before anything is served (#1840).
func TestVerifyProvesTheVolumeArbitrates(t *testing.T) {
	dir := t.TempDir()
	for _, m := range []Method{MethodFlock, MethodFcntl, MethodDotlock} {
		if err := Verify(dir, m); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".yarilo-lock-probe")); !os.IsNotExist(err) {
			t.Errorf("%s left its probe behind", m)
		}
	}
	if err := Verify(filepath.Join(dir, "nope"), MethodFlock); err == nil {
		t.Error("a directory that is not there passed the check")
	}
}
