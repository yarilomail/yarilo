package mdbox

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

var errDiskSync = errors.New("disk refused the sync")

// failSyncs makes the durability calls fail from here on.
func failSyncs(t *testing.T, file, dir bool) {
	t.Helper()
	prevFile, prevDir := syncFile, syncDir
	if file {
		syncFile = func(*os.File) error { return errDiskSync }
	}
	if dir {
		syncDir = func(string) error { return errDiskSync }
	}
	t.Cleanup(func() { syncFile, syncDir = prevFile, prevDir })
}

func mapRecords(t *testing.T, u *userMailbox) int {
	t.Helper()
	m, err := u.openMap()
	if err != nil {
		t.Fatal(err)
	}
	recs, err := m.RecordsReloaded()
	if err != nil {
		t.Fatal(err)
	}
	return len(recs)
}

// A save whose bytes the disk did not make durable fails, and the map does not
// name them: an entry outliving its bytes is a message nobody can read (#1847).
func TestASaveTheDiskCannotMakeDurableIsNotNamed(t *testing.T) {
	for name, c := range map[string]struct {
		mode      mailbox.FsyncMode
		file, dir bool
	}{
		"body sync":      {mailbox.FsyncOptimized, true, false},
		"directory sync": {mailbox.FsyncAlways, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			u, _ := fsyncUser(t, c.mode)
			failSyncs(t, c.file, c.dir)
			body := "Subject: s\r\n\r\nunsynced\r\n"
			_, _, _, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
			if !errors.Is(err, errDiskSync) {
				t.Fatalf("save err %v, want the sync failure", err)
			}
			if n := mapRecords(t, u); n != 0 {
				t.Errorf("the map names %d records after a failed save", n)
			}
		})
	}
}

// A compaction the disk cannot make durable fails, leaves no half-made file,
// and the mail reads from where it was.
func TestARewriteTheDiskCannotMakeDurableLeavesTheStore(t *testing.T) {
	t.Run("purge", func(t *testing.T) {
		u := openTestUserMailbox(t, t.TempDir())
		gone := testSaveMsg(t, u, "Subject: g\r\n\r\ngone\r\n")
		live := testSaveMsg(t, u, "Subject: l\r\n\r\nlive\r\n")
		if err := u.Remove("INBOX", gone); err != nil {
			t.Fatal(err)
		}
		failSyncs(t, true, false)
		if _, err := u.Purge(); !errors.Is(err, errDiskSync) {
			t.Fatalf("purge err %v, want the sync failure", err)
		}
		if got := mfiles(t, u.storagePath()); len(got) != 1 {
			t.Errorf("m-files %v after a failed purge, want m.1 alone", got)
		}
		if b := testFetchBody(t, u, live); !strings.Contains(b, "live") {
			t.Errorf("live message after the failed purge: %q", b)
		}
	})
	t.Run("alt move", func(t *testing.T) {
		_, u, _ := altTestBackend(t)
		name := saveReceivedAt(t, u, "Subject: o\r\n\r\nold\r\n", time.Now().Add(-48*time.Hour))
		failSyncs(t, true, false)
		if _, err := u.AltMove(AltMoveQuery{Before: time.Now()}); !errors.Is(err, errDiskSync) {
			t.Fatalf("alt move err %v, want the sync failure", err)
		}
		if got := mfiles(t, u.altStoragePath()); len(got) != 0 {
			t.Errorf("alt m-files %v after a failed move", got)
		}
		if b := testFetchBody(t, u, name); !strings.Contains(b, "old") {
			t.Errorf("message after the failed move: %q", b)
		}
	})
}
