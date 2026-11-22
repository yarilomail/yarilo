package mailboxbase_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// statusFolder is a folder of 200 messages, adopted by one walk, whose cur/
// and new/ have not moved since.
func statusFolder(t *testing.T) (home string, open func() mailbox.Box) {
	t.Helper()
	box, inbox := gateSetup(t)
	at := time.Now().Add(-time.Hour)
	for i := 0; i < 200; i++ {
		deliverOutOfBand(t, inbox, fmt.Sprintf("17000%05d.M1P1.h:2,", i), at)
	}
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatal(err)
	}
	if n := messageCount(t, box); n != 200 {
		t.Fatalf("the adopting walk took %d messages, want 200", n)
	}
	home = filepath.Dir(inbox)
	open = func() mailbox.Box {
		info := &mailbox.UserInfo{Username: "u@x.com", Home: home}
		store := maildir.New().OpenUser(info)
		idx := fileidx.New().OpenUser(info)
		t.Cleanup(func() { store.Close(); idx.Close() }) //nolint:errcheck
		return mailboxbase.Open(store, idx)
	}
	return home, open
}

// STATUS opens the folder, and an open walks cur/ only when the folder moved:
// a folder nobody changed costs no walk in this process, in another one, or
// to a crowd of sessions arriving at once (#2661). The reference keeps the
// directories' times in the index, so every process sees the last walk.
func TestAStatusOnAnUnchangedFolderWalksNothing(t *testing.T) {
	tests := []struct {
		name     string
		sessions int
		fresh    bool // another process: no walk remembered in memory
		want     float64
	}{
		{"the same process again", 1, false, 0},
		{"another process", 1, true, 0},
		{"eight sessions of another process at once", 8, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, open := statusFolder(t)
			if tc.fresh {
				t.Cleanup(mailboxbase.SetTestSyncTokens(8))
			}
			boxes := make([]mailbox.Box, tc.sessions)
			for i := range boxes {
				boxes[i] = open()
			}
			before := syncCount(t, "scanned")
			var wg sync.WaitGroup
			for _, b := range boxes {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := b.Folder("INBOX", 0); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if got := syncCount(t, "scanned") - before; got != tc.want {
				t.Errorf("%d STATUS opens of an unchanged folder walked cur/ %v times, want %v", tc.sessions, got, tc.want)
			}
		})
	}
}

// opensAt opens the folder from n sessions of one process at once, with no
// walk remembered in that process's memory, and returns the walks they made.
func opensAt(t *testing.T, open func() mailbox.Box, n int) float64 {
	t.Helper()
	t.Cleanup(mailboxbase.SetTestSyncTokens(8))
	boxes := make([]mailbox.Box, n)
	for i := range boxes {
		boxes[i] = open()
	}
	before := syncCount(t, "scanned")
	var wg sync.WaitGroup
	for _, b := range boxes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Folder("INBOX", 0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	return syncCount(t, "scanned") - before
}

// A write of ours in one process carries the stamp: the others walk the folder
// once, together, and not before the two seconds a change in the same tick of
// the clock could hide in; a file put in from outside costs one walk to all
// (#2661). Processes here come one after another.
func TestAWriteCostsOneWalkToEveryProcess(t *testing.T) {
	t.Cleanup(func() { mailbox.SyncClock = time.Now })
	writes := []struct {
		name  string
		write func(t *testing.T, b *mailboxbase.Box, f *mailbox.Folder) int // the messages after it
	}{
		{"a save", func(t *testing.T, b *mailboxbase.Box, f *mailbox.Folder) int {
			const body = "From: a@b\r\n\r\nours\r\n"
			saved, vsize, guid, err := b.Store().Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.RecordSaved(f, "INBOX", saved, &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}); err != nil {
				t.Fatal(err)
			}
			return 201
		}},
		{"a flag change", func(t *testing.T, b *mailboxbase.Box, f *mailbox.Folder) int {
			msgs, err := b.Index().GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 1}})
			if err != nil || len(msgs) != 1 {
				t.Fatal(err)
			}
			name, err := b.MessagePath("INBOX", msgs[0])
			if err != nil {
				t.Fatal(err)
			}
			res := b.WriteFlags(f, "INBOX", []mailbox.FlagWrite{{UID: msgs[0].UID, Filename: name, Flags: []string{`\Flagged`}}})
			if len(res) != 1 || res[0].Err != nil {
				t.Fatalf("flag write: %+v", res)
			}
			return 200
		}},
		{"an expunge", func(t *testing.T, b *mailboxbase.Box, f *mailbox.Folder) int {
			msgs, err := b.Index().GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 1}})
			if err != nil || len(msgs) != 1 {
				t.Fatal(err)
			}
			if _, failed, err := b.ExpungeMarked(f, "INBOX", msgs); err != nil || failed != 0 {
				t.Fatalf("expunge: %d, %v", failed, err)
			}
			return 199
		}},
	}
	for _, w := range writes {
		t.Run(w.name+" of ours in another process", func(t *testing.T) {
			_, open := statusFolder(t)
			writer := open().(*mailboxbase.Box)
			f, err := writer.Folder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			want := w.write(t, writer, f)
			written := time.Now()
			clock := written
			mailbox.SyncClock = func() time.Time { return clock }
			for _, at := range []int64{0, 1} { // the write's second, and the one after: inside the window
				clock = time.Unix(written.Unix()+at, 0)
				for _, p := range []string{"second", "third"} {
					if got := opensAt(t, open, 8); got != 0 {
						t.Fatalf("%d s after the write, the %s process walked %v times; the window is two seconds", at, p, got)
					}
				}
			}
			clock = time.Unix(written.Unix()+2, 0)
			var total float64
			for _, p := range []string{"second", "third", "fourth"} {
				got := opensAt(t, open, 8)
				total += got
				if p == "fourth" && got != 0 {
					t.Errorf("the open after the walk walked %v times", got)
				}
			}
			if total != 1 {
				t.Errorf("past the window the processes walked %v times together, want 1", total)
			}
			mailbox.SyncClock = time.Now
			if n := messageCount(t, open()); n != want {
				t.Errorf("the folder holds %d messages, want %d", n, want)
			}
		})
	}
	t.Run("a file put in from outside", func(t *testing.T) {
		home, open := statusFolder(t)
		deliverOutOfBand(t, filepath.Join(home, "Maildir"), "1700099999.M9P9.h:2,", time.Now().Add(-time.Minute))
		var total float64
		for range 3 {
			total += opensAt(t, open, 8)
		}
		if total != 1 {
			t.Errorf("a file from outside cost %v walks to three processes of eight sessions, want 1", total)
		}
		if n := messageCount(t, open()); n != 201 {
			t.Errorf("the folder holds %d messages, want 201", n)
		}
	})
}

// walkingChildHome, set, makes this test one of the processes opening at once.
const walkingChildHome = "YARILO_WALKING_CHILD_HOME"

// Processes that arrive at once each decide without a lock, as the reference
// does: a changed folder costs at most one walk per process, never one per
// session (#2661).
func TestProcessesArrivingAtOnceWalkAtMostOnceEach(t *testing.T) {
	if home := os.Getenv(walkingChildHome); home != "" {
		info := &mailbox.UserInfo{Username: "u@x.com", Home: home}
		open := func() mailbox.Box {
			store := maildir.New().OpenUser(info)
			idx := fileidx.New().OpenUser(info)
			t.Cleanup(func() { store.Close(); idx.Close() }) //nolint:errcheck
			return mailboxbase.Open(store, idx)
		}
		fmt.Printf("walks=%v\n", opensAt(t, open, 8))
		return
	}
	home, _ := statusFolder(t)
	deliverOutOfBand(t, filepath.Join(home, "Maildir"), "1700099999.M9P9.h:2,", time.Now().Add(-time.Minute))
	outs := make([][]byte, 3)
	var wg sync.WaitGroup
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessesArrivingAtOnceWalkAtMostOnceEach$", "-test.count=1", "-test.v")
			c.Env = append(os.Environ(), walkingChildHome+"="+home)
			out, err := c.CombinedOutput()
			if err != nil {
				t.Errorf("process %d: %v\n%s", i, err, out)
			}
			outs[i] = out
		}()
	}
	wg.Wait()
	var total float64
	for i, out := range outs {
		_, rest, ok := strings.Cut(string(out), "walks=")
		if !ok {
			t.Fatalf("process %d printed no count:\n%s", i, out)
		}
		n, _ := strconv.ParseFloat(strings.Fields(rest)[0], 64)
		if n > 1 {
			t.Errorf("process %d walked %v times for eight sessions, want at most 1", i, n)
		}
		total += n
	}
	if total < 1 {
		t.Errorf("no process walked the changed folder")
	}
}
