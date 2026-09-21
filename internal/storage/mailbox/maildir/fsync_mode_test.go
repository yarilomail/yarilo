package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// syncLog records what was made durable, by path, in the order it happened.
type syncLog struct {
	files []string
	dirs  []string
}

func watchSyncs(t *testing.T) *syncLog {
	t.Helper()
	l := &syncLog{}
	prevFile, prevDir := syncFile, syncDir
	syncFile = func(f *os.File) error {
		l.files = append(l.files, f.Name())
		return prevFile(f)
	}
	syncDir = func(dir string) error {
		l.dirs = append(l.dirs, dir)
		return prevDir(dir)
	}
	t.Cleanup(func() { syncFile, syncDir = prevFile, prevDir })
	return l
}

// inDir counts the synced files whose directory is exactly dir: a substring
// match also counts the runner's own temp root, which lives under /tmp (#1847).
func (l *syncLog) inDir(dir string) int {
	n := 0
	for _, p := range l.files {
		if filepath.Dir(p) == dir {
			n++
		}
	}
	return n
}

// saveOne runs a delivery the way LMTP does: body, then the name and the row.
func saveOne(t *testing.T, box *userMailbox, uid uint32) string {
	t.Helper()
	body := "From: a@b\r\nSubject: m\r\n\r\nbody\r\n"
	saved, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	named, aerr := box.AssignUID("INBOX", saved, uid)
	if aerr != nil {
		t.Fatal(aerr)
	}
	return named
}

func fsyncBox(t *testing.T, mode mailbox.FsyncMode) *userMailbox {
	t.Helper()
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: testHome(root, "u@x.com")}
	box := New(WithFsync(mode)).OpenUser(info).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	return box
}

// optimized makes the body durable before it is published, and leaves the
// index to the crash recovery the journal already gives (#1847).
func TestOptimizedSyncsTheBodyBeforeItIsPublished(t *testing.T) {
	box := fsyncBox(t, mailbox.FsyncOptimized)
	l := watchSyncs(t)
	saveOne(t, box, 1)

	tmpDir := filepath.Join(box.folderPath("INBOX"), "tmp")
	if got := l.inDir(tmpDir); got != 1 {
		t.Errorf("the body was synced %d times, want 1: %v", got, l.files)
	}
	if got := len(l.dirs); got != 0 {
		t.Errorf("optimized synced %d directories, want none: %v", got, l.dirs)
	}
	// The order is the property: a sync after the name is a sync after the
	// client has been told.
	if len(l.files) == 0 || filepath.Dir(l.files[0]) != tmpDir {
		t.Errorf("the first sync was %v, want the body in %s", l.files, tmpDir)
	}
}

// never syncs nothing on the delivery path, the list row included.
func TestNeverSyncsNothingOnTheDeliveryPath(t *testing.T) {
	box := fsyncBox(t, mailbox.FsyncNever)
	l := watchSyncs(t)
	saveOne(t, box, 1)

	if len(l.files) != 0 || len(l.dirs) != 0 {
		t.Errorf("never synced %v and %v, want nothing", l.files, l.dirs)
	}
}

// always adds the directory entry after the file is published.
func TestAlwaysSyncsTheDirectoryAfterPublishing(t *testing.T) {
	box := fsyncBox(t, mailbox.FsyncAlways)
	l := watchSyncs(t)
	saveOne(t, box, 1)

	if got := len(l.dirs); got != 1 {
		t.Fatalf("always synced %d directories, want 1: %v", got, l.dirs)
	}
	// new/, because a save with no flags is a delivery and that is where one
	// waits until a sync moves it (#1959).
	if want := filepath.Join(box.folderPath("INBOX"), "new"); l.dirs[0] != want {
		t.Errorf("the directory synced was %q, want %q", l.dirs[0], want)
	}
}

// The same save with a flag is published into cur/, and that is the directory
// made durable: the name decides where the file goes (#1959).
func TestAlwaysSyncsCurWhenTheNameCarriesFlags(t *testing.T) {
	box := fsyncBox(t, mailbox.FsyncAlways)
	l := watchSyncs(t)
	body := "From: a@b\r\n\r\nx\r\n"
	name, _, _, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), []string{`\Seen`}, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := box.AssignUID("INBOX", name, 1); aerr != nil {
		t.Fatal(aerr)
	}
	if got := len(l.dirs); got == 0 {
		t.Fatal("no directory was synced at all")
	}
	if want := filepath.Join(box.folderPath("INBOX"), "cur"); l.dirs[0] != want {
		t.Errorf("the directory synced was %q, want %q", l.dirs[0], want)
	}
}
