package maildir

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func item1Folder(t *testing.T, messages int) (*userMailbox, string) {
	t.Helper()
	home := t.TempDir()
	box := New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	t.Cleanup(func() { _ = box.Close() })
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	u := mailbox.Driver(box).(*userMailbox)

	cur := filepath.Join(home, "Maildir", "cur")
	for i := 1; i <= messages; i++ {
		name := fmt.Sprintf("17000000%02d.M%dP1.host,S=20,W=20:2,", i, i)
		if err := os.WriteFile(filepath.Join(cur, name), []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := u.AssignUID("INBOX", name, uint32(i)); err != nil {
			t.Fatal(err)
		}
	}
	return u, home
}

// A row appended to the list is read without parsing the rows before it: a
// folder of ten thousand re-read the whole file for one new message (#1875).
func TestAnAppendedRowIsReadWithoutTheHead(t *testing.T) {
	u, home := item1Folder(t, 20)
	if _, err := u.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}

	wholeBefore := listReads.Load()
	tailBefore := listAppendReads.Load()

	// Another process appended, which is what leaves our map behind while the
	// file keeps its inode: our own AssignUID updates the map and never parses.
	name := "1700000099.M99P1.host,S=20,W=20:2,"
	if err := os.WriteFile(filepath.Join(home, "Maildir", "cur", name), []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := os.OpenFile(filepath.Join(home, "Maildir", "yarilo-uidlist"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(list, "99 :%s\n", name); err != nil {
		t.Fatal(err)
	}
	if err := list.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := u.readUIDList("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if got := got[maildirBase(name)]; got != 99 {
		t.Fatalf("the appended row is not in the map: uid %d", got)
	}
	if got[maildirBase("1700000001.M1P1.host,S=20,W=20:2,")] != 1 {
		t.Error("the rows read before the append are gone")
	}
	if now := listAppendReads.Load(); now != tailBefore+1 {
		t.Errorf("tail reads = %d, want %d: the whole file was parsed again", now-tailBefore, 1)
	}
	if now := listReads.Load(); now != wholeBefore {
		t.Errorf("the head was parsed again: whole reads %d -> %d", wholeBefore, now)
	}
}

// A list that was replaced, not appended to, is read whole: a new inode is a
// different file, whatever its size says.
func TestAReplacedListIsReadWhole(t *testing.T) {
	u, home := item1Folder(t, 5)
	if _, err := u.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "Maildir", "yarilo-uidlist")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Written under another name and renamed in, as every rewrite is.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, []byte("6 :1700000006.M6P1.host,S=20,W=20:2,\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	wholeBefore, tailBefore := listReads.Load(), listAppendReads.Load()
	if _, err := u.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}
	if listAppendReads.Load() != tailBefore {
		t.Error("a file with a new inode was read as an append")
	}
	if listReads.Load() != wholeBefore+1 {
		t.Error("a replaced list was not read whole")
	}
}

// After this process renames a file for a flag change, finding that message
// again reads no directory: the rename is put into the listing it already has.
func TestOurOwnFlagRenameDoesNotCostAReadDir(t *testing.T) {
	u, _ := item1Folder(t, 10)
	const base = "1700000003.M3P1.host,S=20,W=20:2,"
	if _, err := u.currentName("INBOX", maildirBase(base)); err != nil {
		t.Fatal(err)
	}

	before := dirReads.Load()
	renamed, err := u.writeFlagsLocked("INBOX", base, []string{`\Seen`}, "")
	if err != nil {
		t.Fatal(err)
	}
	if renamed == base {
		t.Fatal("the flag change renamed nothing, so the row asserts nothing")
	}
	got, err := u.currentName("INBOX", maildirBase(base))
	if err != nil {
		t.Fatalf("the message is not findable after its own rename: %v", err)
	}
	if got != renamed {
		t.Errorf("currentName says %q, the file is %q", got, renamed)
	}
	if now := dirReads.Load(); now != before {
		t.Errorf("cur/ was read %d times after our own rename, want 0", now-before)
	}
}
