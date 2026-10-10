package maildir

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// statAs reports a file's real stat with another inode number: the inode a
// rename-over can hand to a new list once the old one is let go.
type statAs struct {
	os.FileInfo
	ino uint64
}

func (s statAs) Sys() any {
	st := *s.FileInfo.Sys().(*syscall.Stat_t)
	st.Ino = s.ino
	return &st
}

func heldList(t *testing.T) (*userMailbox, string) {
	t.Helper()
	box, _, _ := recSetup(t)
	if _, err := box.AssignUID("INBOX", "1700000001.M1P1.h,S=3", 1); err != nil {
		t.Fatal(err)
	}
	box = New().OpenUser(&mailbox.UserInfo{Username: box.username, Home: box.home}).(*userMailbox)
	t.Cleanup(func() { box.Close() })
	if _, _, err := box.readUIDListFrom("INBOX"); err != nil {
		t.Fatal(err)
	}
	return box, filepath.Join(box.controlFolderPath("INBOX"), UIDListFileName)
}

// replaceList renames a new list over the path, as every rewrite does.
func replaceList(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path+".new", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
}

func header(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range raw {
		if b == '\n' {
			return string(raw[:i+1])
		}
	}
	t.Fatal("no header line")
	return ""
}

// A list recreated under the inode number of the one the map was read from is
// read whole: the descriptor still on the old file says its size, and the tail
// of the new one would drop the rows before it (#2183).
func TestARecreatedListUnderTheSameInodeIsReadWhole(t *testing.T) {
	box, path := heldList(t)
	held := box.folderCacheFor("INBOX").fdIno
	oldFD := box.folderCacheFor("INBOX").listFD
	replaceList(t, path, header(t, path)+
		"1 :1700000001.M1P1.h,S=3\n"+
		"2 :1700000002.M2P1.h,S=3\n"+
		"3 :1700000003.M3P1.h,S=3\n")

	prev := statPath
	statPath = func(name string) (os.FileInfo, error) {
		fi, err := prev(name)
		if err != nil || name != path {
			return fi, err
		}
		return statAs{fi, held}, nil
	}
	defer func() { statPath = prev }()

	m, read, err := box.readUIDListFrom("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["1700000002.M2P1.h,S=3"]; !ok || len(m) != 3 {
		t.Errorf("map after a recreate = %v (from %s), want all three rows", m, read.from)
	}
	if _, err := oldFD.Stat(); err == nil {
		t.Error("the descriptor on the replaced list is still open")
	}
}

// Same size, another file: only the inode the descriptor holds tells them
// apart, and the path's file is the one read.
func TestAListOfTheSameSizeOnAnotherInodeIsReadWhole(t *testing.T) {
	box, path := heldList(t)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("2 :1700000002.M2P1.h,S=3\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// The held file has grown to exactly this size too.
	replaceList(t, path, header(t, path)+
		"1 :1700000001.M1P1.h,S=3\n"+
		"7 :1700000007.M7P1.h,S=3\n")

	m, _, err := box.readUIDListFrom("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if m["1700000007.M7P1.h,S=3"] != 7 || len(m) != 2 {
		t.Errorf("map = %v, want the path's rows 1 and 7", m)
	}
}

// The tail is read through the descriptor the map was read with: a path that
// names another file by the time of the read must not lend its bytes.
func TestTheTailIsReadThroughTheHeldDescriptor(t *testing.T) {
	box, path := heldList(t)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("2 :1700000002.M2P1.h,S=3\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	appended, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Renamed over between the stat and the read.
	replaceList(t, path, header(t, path)+
		"1 :1700000001.M1P1.h,S=3\n"+
		"9 :1700000009.M9P1.h,S=3\n")

	prev := statPath
	statPath = func(name string) (os.FileInfo, error) {
		if name == path {
			return appended, nil
		}
		return prev(name)
	}
	defer func() { statPath = prev }()

	m, read, err := box.readUIDListFrom("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if read.from != "tail" {
		t.Fatalf("from = %s, want tail", read.from)
	}
	if m["1700000002.M2P1.h,S=3"] != 2 || len(m) != 2 {
		t.Errorf("map = %v, want rows 1 and 2 of the held file", m)
	}
}

// Close lets go of every list descriptor the caches hold.
func TestCloseReleasesTheHeldListDescriptors(t *testing.T) {
	box, _ := heldList(t)
	fd := box.folderCacheFor("INBOX").listFD
	if fd == nil {
		t.Fatal("a read holds no descriptor")
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fd.Stat(); err == nil {
		t.Error("the descriptor is still open after Close")
	}
	if _, _, err := box.readUIDListFrom("INBOX"); err != nil {
		t.Errorf("a read after Close: %v", err)
	}
	fd = box.folderCacheFor("INBOX").listFD
	box.folderCacheFor("INBOX").invalidateUIDs("test")
	if _, err := fd.Stat(); err == nil {
		t.Error("the descriptor is still open after the map was dropped")
	}
}

// One user holds at most heldListsPerUser list descriptors: the least recently
// read folder lets go of map and descriptor, and is read whole next time.
func TestHeldListsAreBoundedPerUser(t *testing.T) {
	setup, _, _ := recSetup(t)
	folders := make([]string, heldListsPerUser+1)
	for i := range folders {
		folders[i] = fmt.Sprintf("F%02d", i)
		if err := setup.Create(folders[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := setup.AssignUID(folders[i], "1700000001.M1P1.h,S=3", 1); err != nil {
			t.Fatal(err)
		}
	}
	box := New().OpenUser(&mailbox.UserInfo{Username: setup.username, Home: setup.home}).(*userMailbox)
	t.Cleanup(func() { box.Close() })
	read := func(f string) listRead {
		t.Helper()
		_, r, err := box.readUIDListFrom(f)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, f := range folders[:heldListsPerUser] {
		read(f)
	}
	// Read again before the one past the bound: the oldest is now F01.
	if r := read(folders[0]); r.from != "cache" {
		t.Fatalf("F00 again: from %q, want cache", r.from)
	}
	read(folders[heldListsPerUser])
	held := 0
	for _, f := range folders {
		if box.folderCacheFor(f).listFD != nil {
			held++
		}
	}
	if held != heldListsPerUser {
		t.Errorf("%d folders read hold %d descriptors, want %d", len(folders), held, heldListsPerUser)
	}
	if box.folderCacheFor(folders[0]).listFD == nil {
		t.Error("F00, read just before, was evicted")
	}
	if r := read(folders[1]); r.from != "file" {
		t.Errorf("the evicted F01's next read: from %q, want file", r.from)
	}
}
