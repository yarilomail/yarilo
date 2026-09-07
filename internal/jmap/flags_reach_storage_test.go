package jmap

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// curName is the one file in INBOX/cur, whose trailer carries the flags.
func curName(t *testing.T, home string) string {
	t.Helper()
	entries, err := os.ReadDir(maildirCur(home))
	if err != nil {
		t.Fatalf("read cur/: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("cur/ holds %d files, want 1", len(entries))
	}
	return entries[0].Name()
}

// reconcile runs the pass that takes the flags off the name, which is where a
// write that stopped at the index is undone.
func reconcile(t *testing.T, home string) {
	t.Helper()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	ui := file.New().OpenUser(info)
	defer ui.Close() //nolint:errcheck
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	syncer, ok := mailbox.Driver(box).(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	if !ok {
		t.Fatal("the maildir driver does not reconcile")
	}
	if _, err := syncer.ReconcileIndex(ui, f); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// A keyword set over JMAP reaches the name, which on maildir is where the flags
// live: one that stopped at the index is reverted by the next reconcile (#1724).
func TestAJMAPKeywordReachesTheNameAndSurvivesAReconcile(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, setTestMessage, 0)

	emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords/$flagged":true}}}`, testUser, id))

	name := curName(t, home)
	if _, trailer, ok := strings.Cut(name, ":2,"); !ok || !strings.Contains(trailer, "F") {
		t.Fatalf("the file is named %q; the trailer does not carry \\Flagged", name)
	}

	reconcile(t, home)
	flags, _ := storedFlags(t, home)
	if !slices.Contains(flags, `\Flagged`) {
		t.Errorf("after the reconcile the record holds %v, want \\Flagged among them", flags)
	}
}

// A rename that cannot land leaves the record dirty, and a dirty record keeps
// its own flags: the name is the truth only where the name could be written.
func TestAFlagWriteThatCannotLandLeavesTheRecordDirty(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, setTestMessage, 0)
	cur := maildirCur(home)
	before := curName(t, home)

	if err := os.Chmod(cur, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cur, 0o700) }) //nolint:errcheck

	emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords/$flagged":true}}}`, testUser, id))
	if got := curName(t, home); got != before {
		t.Fatalf("the file was renamed to %q under a directory that forbids it", got)
	}

	if err := os.Chmod(cur, 0o700); err != nil {
		t.Fatal(err)
	}
	reconcile(t, home)
	flags, _ := storedFlags(t, home)
	if !slices.Contains(flags, `\Flagged`) {
		t.Errorf("after the reconcile the record holds %v; a dirty record keeps its flags", flags)
	}
}

// maildirCur is INBOX's cur/ under the user's home, wherever the layout puts it.
func maildirCur(home string) string { return filepath.Join(home, "Maildir", "cur") }
