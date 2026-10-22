package counters

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestBumpRecordsEachTypeAtTheNewValue(t *testing.T) {
	dir := t.TempDir()
	c := New(dir, "u", "o", nil)
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
		if err := c.Bump(st.types...); err != nil {
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
			if err := New(dir, "u", "o", nil).Bump(typ); err != nil {
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
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %d entries", len(entries))
	}
}
