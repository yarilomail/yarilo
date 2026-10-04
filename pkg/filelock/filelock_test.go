package filelock

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/sys/unix"
)

// refuseMethod makes the kernel answer as a volume with no lock daemon does.
func refuseMethod(err error) func() {
	prev := tryLockFn
	tryLockFn = func(*os.File, Method) error { return err }
	return func() { tryLockFn = prev }
}

// forgetRefusals drops what earlier rows learned about this stand's device, so
// one row's refusal is not another's starting state.
func forgetRefusals() {
	refusedMu.Lock()
	refused = map[uint64]bool{}
	refusedMu.Unlock()
}

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

// A reader's open and close must not drop the writer's lock. A POSIX record
// lock does exactly that, which is why flock is the default (#1840).
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

// A volume that refuses the configured method is served by dotlock rather than
// refusing the write, and the exclusion still holds (#1850).
func TestAVolumeThatRefusesTheMethodFallsBackToDotlock(t *testing.T) {
	forgetRefusals()
	defer forgetRefusals()
	dir := t.TempDir()
	path := filepath.Join(dir, "list")

	restore := refuseMethod(unix.ENOLCK)
	defer restore()
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	before := testutil.ToFloat64(metricFallback.WithLabelValues(string(MethodFlock)))

	h, err := Take(path, MethodFlock, time.Second)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if h.method != MethodDotlock {
		t.Fatalf("the hold is %q, want dotlock", h.method)
	}
	if _, serr := os.Stat(path + ".lock"); serr != nil {
		t.Errorf("no dotlock file beside %s: %v", path, serr)
	}

	// Still exclusive: a second taker waits rather than being handed the file.
	second := make(chan error, 1)
	go func() {
		h2, err2 := Take(path, MethodFlock, 200*time.Millisecond)
		if h2 != nil {
			_ = h2.Release()
		}
		second <- err2
	}()
	select {
	case err2 := <-second:
		if !errors.Is(err2, ErrBusy) {
			t.Errorf("the second taker got %v, want ErrBusy", err2)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the second taker neither waited nor gave up")
	}
	_ = h.Release()

	if got := testutil.ToFloat64(metricFallback.WithLabelValues(string(MethodFlock))) - before; got != 1 {
		t.Errorf("the fallback was counted %v times, want 1", got)
	}
	line := logged.String()
	for _, want := range []string{"dotlock", "storage_lock_method"} {
		if !strings.Contains(line, want) {
			t.Errorf("the warning %q does not name %q", line, want)
		}
	}
}

// One line per volume, not per directory: a maildir folder is a directory and
// there are thousands of them on one mount (#1850).
func TestTheWarningIsOncePerVolume(t *testing.T) {
	forgetRefusals()
	defer forgetRefusals()
	root := t.TempDir()
	restore := refuseMethod(unix.ENOLCK)
	defer restore()
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	before := testutil.ToFloat64(metricFallback.WithLabelValues(string(MethodFlock)))

	// Two folders, two directories, one device.
	for _, folder := range []string{"INBOX", "Sent"} {
		dir := filepath.Join(root, folder)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		h, err := Take(filepath.Join(dir, "list"), MethodFlock, time.Second)
		if err != nil {
			t.Fatalf("take in %s: %v", folder, err)
		}
		_ = h.Release()
	}
	if got := strings.Count(logged.String(), "refuses the configured lock method"); got != 1 {
		t.Errorf("two directories on one volume logged %d warnings, want 1", got)
	}
	if got := testutil.ToFloat64(metricFallback.WithLabelValues(string(MethodFlock))) - before; got != 1 {
		t.Errorf("the fallback was counted %v times, want 1", got)
	}
}

// A volume that refused once keeps dotlock: a process alternating methods
// holds path.lock against a sibling's flock on path (#1850).
func TestAVolumeThatRefusedOnceStaysOnDotlock(t *testing.T) {
	forgetRefusals()
	defer forgetRefusals()
	dir := t.TempDir()

	restore := refuseMethod(unix.ENOLCK)
	first, err := Take(filepath.Join(dir, "a"), MethodFlock, time.Second)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	_ = first.Release()
	restore() // the kernel would grant flock again

	second, err := Take(filepath.Join(dir, "b"), MethodFlock, time.Second)
	if err != nil {
		t.Fatalf("take after the refusal: %v", err)
	}
	defer second.Release() //nolint:errcheck
	if second.method != MethodDotlock {
		t.Errorf("the hold after a refused volume is %q, want dotlock", second.method)
	}
}
