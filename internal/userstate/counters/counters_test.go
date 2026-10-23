package counters

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

func TestBumpRecordsEachTypeAtTheNewValue(t *testing.T) {
	dir := t.TempDir()
	c := New(dir, "u")
	steps := []struct {
		types []Type
		want  Snapshot
	}{
		{[]Type{Email, Mailbox}, Snapshot{1, map[Type]uint64{Email: 1, Mailbox: 1}}},
		{[]Type{Identity}, Snapshot{2, map[Type]uint64{Email: 1, Mailbox: 1, Identity: 2}}},
		{[]Type{Email}, Snapshot{3, map[Type]uint64{Email: 3, Mailbox: 1, Identity: 2}}},
		{nil, Snapshot{3, map[Type]uint64{Email: 3, Mailbox: 1, Identity: 2}}},
	}
	for i, st := range steps {
		if err := c.Bump("test", st.types...); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		got, err := Read(Path(dir))
		if err != nil {
			t.Fatalf("step %d: read: %v", i, err)
		}
		if !reflect.DeepEqual(got, st.want) {
			t.Fatalf("step %d: got %+v, want %+v", i, got, st.want)
		}
	}
}

func TestChangedAfter(t *testing.T) {
	s := Snapshot{Highest: 7, ByType: map[Type]uint64{Email: 7, Mailbox: 4, Identity: 2}}
	cases := []struct {
		since uint64
		want  []Type
	}{
		{0, []Type{Email, Identity, Mailbox}},
		{2, []Type{Email, Mailbox}},
		{4, []Type{Email}},
		{7, nil},
	}
	for _, tc := range cases {
		if got := s.ChangedAfter(tc.since); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("since %d: got %v, want %v", tc.since, got, tc.want)
		}
	}
}

func TestMissingFileReadsAsZero(t *testing.T) {
	s, err := Read(filepath.Join(t.TempDir(), FileName))
	if err != nil || s.Highest != 0 || len(s.ByType) != 0 {
		t.Fatalf("got %+v, %v", s, err)
	}
}

// Two counters over one file stand for two sessions: no bump may be lost.
func TestConcurrentBumpsAreNotLost(t *testing.T) {
	dir := t.TempDir()
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			typ := Email
			if i%2 == 1 {
				typ = Mailbox
			}
			if err := New(dir, "u").Bump("test", typ); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	s, err := Read(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if s.Highest != n {
		t.Fatalf("highest %d, want %d", s.Highest, n)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{FileName, FileName + ".lock"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("left behind %v, want %v", names, want)
	}
}

// Two processes bumping one account's counter at once lose nothing: the
// process mutex cannot see the other one, the file lock can.
func TestBumpsFromTwoProcessesAreNotLost(t *testing.T) {
	if dir := os.Getenv("COUNTERS_BUMP_DIR"); dir != "" {
		for i := 0; i < 200; i++ {
			if err := New(dir, "u").Bump("test", Email); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	var cmds []*exec.Cmd
	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBumpsFromTwoProcessesAreNotLost$")
		cmd.Env = append(os.Environ(), "COUNTERS_BUMP_DIR="+dir)
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Read(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if s.Highest != 400 {
		t.Fatalf("highest %d after 2 x 200 bumps", s.Highest)
	}
}

// A process touching many accounts keeps at most maxKept lock files open, and
// a counter whose state was closed still bumps from where it was.
func TestIdleLockFilesAreClosedPastTheBound(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	if err := New(first, "u").Bump("test", Email); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxKept+10; i++ {
		if err := New(filepath.Join(root, strconv.Itoa(i)), "u").Bump("test", Email); err != nil {
			t.Fatal(err)
		}
	}
	statesMu.Lock()
	n := len(states)
	statesMu.Unlock()
	if n > maxKept {
		t.Fatalf("%d lock files kept open, bound %d", n, maxKept)
	}
	if err := New(first, "u").Bump("test", Mailbox); err != nil {
		t.Fatal(err)
	}
	s, err := Read(Path(first))
	if err != nil || s.Highest != 2 {
		t.Fatalf("after reopening: %+v, %v", s, err)
	}
}

// Closing idle lock files must never close one a bump holds: that would drop
// the fcntl lock mid-bump and let the other process interleave.
func TestClosingIdleLockFilesKeepsAHeldOne(t *testing.T) {
	if dir := os.Getenv("COUNTERS_CHURN_DIR"); dir != "" {
		maxKept = 4
		done := make(chan struct{})
		go func() {
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				_ = New(filepath.Join(dir, "churn", strconv.Itoa(os.Getpid()), strconv.Itoa(i%64)), "u").Bump("test", Email)
			}
		}()
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 75; i++ {
					if err := New(filepath.Join(dir, "shared"), "u").Bump("test", Email); err != nil {
						fmt.Fprintln(os.Stderr, err)
						os.Exit(1)
					}
				}
			}()
		}
		wg.Wait()
		close(done)
		os.Exit(0)
	}
	dir := t.TempDir()
	var cmds []*exec.Cmd
	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestClosingIdleLockFilesKeepsAHeldOne$")
		cmd.Env = append(os.Environ(), "COUNTERS_CHURN_DIR="+dir)
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Read(Path(filepath.Join(dir, "shared")))
	if err != nil {
		t.Fatal(err)
	}
	if s.Highest != 600 {
		t.Fatalf("highest %d after 2 x 300 bumps under eviction pressure", s.Highest)
	}
}
