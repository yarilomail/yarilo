package mdbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// syncLog records what was made durable and in what order.
type syncLog struct {
	files []string
	dirs  []string
	after []string // what the map recorded, interleaved with the syncs
}

func watchSyncs(t *testing.T) *syncLog {
	t.Helper()
	l := &syncLog{}
	prevFile, prevDir, prevMap := syncFile, syncDir, afterMapAppend
	syncFile = func(f *os.File) error {
		l.files = append(l.files, f.Name())
		l.after = append(l.after, "sync:"+filepath.Base(f.Name()))
		return prevFile(f)
	}
	syncDir = func(dir string) error {
		l.dirs = append(l.dirs, dir)
		l.after = append(l.after, "syncdir")
		return prevDir(dir)
	}
	afterMapAppend = func() { l.after = append(l.after, "map") }
	t.Cleanup(func() { syncFile, syncDir, afterMapAppend = prevFile, prevDir, prevMap })
	return l
}

func fsyncUser(t *testing.T, mode mailbox.FsyncMode) (*userMailbox, string) {
	t.Helper()
	home := t.TempDir()
	u := New(WithFsync(mode)).OpenUser(&mailbox.UserInfo{
		Username: "alice@example.com", Home: home,
	}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	return u, home
}

func saveOne(t *testing.T, u *userMailbox) {
	t.Helper()
	body := "From: a@b\r\nSubject: m\r\n\r\nbody\r\n"
	if _, _, _, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
}

// optimized syncs the storage file before the map names it: a map entry that
// outlives the bytes points at a message nobody can read (#1847).
func TestOptimizedSyncsTheFileBeforeTheMapNamesIt(t *testing.T) {
	u, _ := fsyncUser(t, mailbox.FsyncOptimized)
	l := watchSyncs(t)
	saveOne(t, u)

	if len(l.files) != 1 || !strings.HasPrefix(filepath.Base(l.files[0]), "m.") {
		t.Fatalf("the synced files are %v, want one m.N", l.files)
	}
	if len(l.dirs) != 0 {
		t.Errorf("optimized synced %v, want no directory", l.dirs)
	}
	if got := strings.Join(l.after, ","); !strings.HasPrefix(got, "sync:m.") || !strings.HasSuffix(got, "map") {
		t.Errorf("the order was %q, want the sync before the map append", got)
	}
}

// never syncs nothing on the delivery path.
func TestNeverSyncsNothingOnTheSavePath(t *testing.T) {
	u, _ := fsyncUser(t, mailbox.FsyncNever)
	l := watchSyncs(t)
	saveOne(t, u)

	if len(l.files) != 0 || len(l.dirs) != 0 {
		t.Errorf("never synced %v and %v, want nothing", l.files, l.dirs)
	}
}

// always adds the storage directory, but only where the save created the file:
// the entry is what a crash loses, and an existing file has one already.
func TestAlwaysSyncsTheStorageDirectoryOnceForANewFile(t *testing.T) {
	u, _ := fsyncUser(t, mailbox.FsyncAlways)
	l := watchSyncs(t)
	saveOne(t, u)

	if len(l.dirs) != 1 || l.dirs[0] != u.storagePath() {
		t.Fatalf("always synced %v, want %q once", l.dirs, u.storagePath())
	}

	// The second message lands in the file the first created.
	before := len(l.dirs)
	saveOne(t, u)
	if got := len(l.dirs) - before; got != 0 {
		t.Errorf("a save into an existing m.N synced the directory %d times, want 0", got)
	}
}
