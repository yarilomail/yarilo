package maildir

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// rewriteList renames over the list the way a writer does, and pins the mtime:
// two rewrites of one length then collide on the old key.
func rewriteList(t *testing.T, path, base string) {
	t.Helper()
	body := "3 V1 N9 G00000000000000000000000000000000\n1 :" + base + "\n"
	tmp := path + ".rewrite"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	when := time.Unix(1700000000, 0)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// Two rewrites of one length in one moment: the reader must see the second.
// Keeping the first makes a named record read as a nameless one (#1739).
func TestARewrittenListIsNotServedFromTheOldSnapshot(t *testing.T) {
	home := t.TempDir()
	u := New().OpenUser(&mailbox.UserInfo{Username: "u@x", Home: home, Separator: "/"}).(*userMailbox)
	t.Cleanup(func() { _ = u.Close() })
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	path := u.uidListPath("INBOX")

	rewriteList(t, path, "1700000001.M1P1.hostA")
	first, err := u.readUIDList("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first["1700000001.M1P1.hostA"]; !ok {
		t.Fatalf("the first read saw %v", names(first))
	}

	rewriteList(t, path, "1700000001.M1P1.hostB")
	second, err := u.readUIDList("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := second["1700000001.M1P1.hostB"]; !ok {
		t.Fatalf("after the second rewrite the reader still sees %s", names(second))
	}
}

// The same for the read a record makes: it asks by uid, through its own path.
func TestARecordReadsTheRewrittenList(t *testing.T) {
	home := t.TempDir()
	u := New().OpenUser(&mailbox.UserInfo{Username: "u@x", Home: home, Separator: "/"}).(*userMailbox)
	t.Cleanup(func() { _ = u.Close() })
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	path := u.uidListPath("INBOX")

	rewriteList(t, path, "1700000001.M1P1.hostA")
	if _, err := u.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}
	rewriteList(t, path, "1700000001.M1P1.hostB")

	base, err := u.baseForUID("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if base != "1700000001.M1P1.hostB" {
		t.Errorf("the record reads base %q, and the list names 1700000001.M1P1.hostB", base)
	}
}

// A snapshot is still served when nothing wrote: the trap must not pass by
// reloading on every call.
func TestAnUntouchedListIsStillServedFromTheSnapshot(t *testing.T) {
	home := t.TempDir()
	u := New().OpenUser(&mailbox.UserInfo{Username: "u@x", Home: home, Separator: "/"}).(*userMailbox)
	t.Cleanup(func() { _ = u.Close() })
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	rewriteList(t, u.uidListPath("INBOX"), "1700000001.M1P1.hostA")
	if _, err := u.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}
	ResetListReads()
	for i := 0; i < 3; i++ {
		if _, err := u.readUIDList("INBOX"); err != nil {
			t.Fatal(err)
		}
	}
	if got := ListReads(); got != 0 {
		t.Errorf("three reads of an untouched list parsed it %d times", got)
	}
}

func names(m map[string]uint32) string {
	out := make([]string, 0, len(m))
	for b := range m {
		out = append(out, b)
	}
	return strings.Join(out, ",")
}

// Two handles, as two sessions are: B writes its row from a snapshot older than
// A's write, and must still read A's row back (#1739).
func TestASecondWriterKeepsTheRowTheFirstWrote(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x", Home: home, Separator: "/"}
	a := New().OpenUser(info).(*userMailbox)
	b := New().OpenUser(info).(*userMailbox)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}

	const body = "From: a@b\r\nSubject: x\r\n\r\nbody\r\n"
	save := func(u *userMailbox, uid uint32) string {
		name, _, _, err := u.Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		return name
	}
	// A row already there, so B's snapshot is a real one and not the empty
	// map a missing list gives.
	first := save(a, 364)
	if _, err := a.AssignUID("INBOX", first, 364); err != nil {
		t.Fatal(err)
	}
	nameA, nameB := save(a, 365), save(b, 366)

	// B reads the list before A names its message: the snapshot it would merge
	// into is one row short.
	if _, err := b.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AssignUID("INBOX", nameA, 365); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AssignUID("INBOX", nameB, 366); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		uid  uint32
		name string
	}{{364, first}, {365, nameA}, {366, nameB}} {
		base, err := b.baseForUID("INBOX", tc.uid)
		if err != nil {
			t.Fatal(err)
		}
		if base != maildirBase(tc.name) {
			t.Errorf("the second writer reads uid %d as %q, and the list names it %q",
				tc.uid, base, maildirBase(tc.name))
		}
	}
}

// The stamp carries the inode and the ctime: without them a stamp that compares
// equal to a changed file cannot be read as anything (#1739).
func TestTheStampCarriesTheInodeAndCtime(t *testing.T) {
	home := t.TempDir()
	u := New().OpenUser(&mailbox.UserInfo{Username: "u@x", Home: home, Separator: "/"}).(*userMailbox)
	t.Cleanup(func() { _ = u.Close() })
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	path := u.uidListPath("INBOX")

	rewriteList(t, path, "1700000001.M1P1.hostA")
	first := u.listStampNow("INBOX")
	if first.inode() == 0 {
		t.Fatal("the stamp names no inode; this filesystem cannot answer the question the row is about")
	}
	if first.ctimeNanos() == 0 {
		t.Error("the stamp names no ctime")
	}

	rewriteList(t, path, "1700000001.M1P1.hostB")
	second := u.listStampNow("INBOX")
	if second.mtime != first.mtime || second.size != first.size {
		t.Skip("the rewrite moved mtime or size; the fixture cannot make the two collide here")
	}
	if second.inode() == first.inode() && second.ctimeNanos() == first.ctimeNanos() {
		t.Error("two rewrites of one length in one moment are indistinguishable: neither the inode nor the ctime moved")
	}
}
