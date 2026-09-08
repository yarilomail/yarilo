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
