package pop3

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const preMigrationBody = "From: a@b\r\nSubject: old\r\n\r\nold body\r\n"

// preMigrationStand is the shape an older build left: names in the index
// sidecar alone, sizes nowhere else, a uid list that never heard of them.
func preMigrationStand(t *testing.T) Options {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x", Home: home, Driver: "maildir"}
	mb := maildir.New()
	box := mb.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	idx := fileindex.New().OpenUser(info)
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	cur := filepath.Join(home, "Maildir", "cur")
	lines := ""
	for i := 1; i <= 2; i++ {
		base := fmt.Sprintf("170000000%d.M1P1_%d.host,S=%d,W=%d",
			i, i, len(preMigrationBody), len(preMigrationBody))
		name := base + ":2,"
		if err := os.WriteFile(filepath.Join(cur, name), []byte(preMigrationBody), 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(base))
		var guid [16]byte
		copy(guid[:], sum[:16])
		if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i), GUID: guid}); err != nil {
			t.Fatal(err)
		}
		lines += fmt.Sprintf("%d\t%s\t%d\n", i, name, len(preMigrationBody))
	}
	dir, ok := idx.(interface{ IndexDirFor(string) string })
	if !ok {
		t.Fatal("the index does not name its own directory")
	}
	sidecar := filepath.Join(dir.IndexDirFor("INBOX"), "yarilo.index.names")
	if err := os.WriteFile(sidecar, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	box.Close() //nolint:errcheck
	idx.Close() //nolint:errcheck

	return newTestOpts(&mockAuth{users: map[string]string{"u@x": "p"}, home: home}, mb, fileindex.New())
}

// A POP3 login settles the account it opens: an account no IMAP client ever
// touched read as zero-sized messages RETR could not fetch (#1778).
func TestAPOP3LoginAdoptsAPreMigrationAccount(t *testing.T) {
	opts := preMigrationStand(t)
	out := runPOP3(t, opts, []string{"STAT", "LIST 1", "RETR 1"})

	wantSize := fmt.Sprint(len(preMigrationBody))
	if want := fmt.Sprintf("+OK 2 %d", 2*len(preMigrationBody)); !strings.HasPrefix(out["STAT"], want) {
		t.Errorf("STAT is %q, want %q", out["STAT"], want)
	}
	if !strings.Contains(out["LIST 1"], wantSize) {
		t.Errorf("LIST 1 is %q, want the message's %s octets", out["LIST 1"], wantSize)
	}
	if !strings.HasPrefix(out["RETR 1"], "+OK") {
		t.Errorf("RETR 1 answered %q", out["RETR 1"])
	}
}
