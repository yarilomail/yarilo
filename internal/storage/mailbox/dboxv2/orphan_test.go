package dboxv2

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// orphanFolder is a folder holding one guid-named file no record points at, aged
// as given: what an interrupted save leaves, and what a crash leaves behind it.
func orphanFolder(t *testing.T, body string, age time.Duration) (mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder, string, string) {
	t.Helper()
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")

	temp, _, guid, serr := mb.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if serr != nil {
		t.Fatal(serr)
	}
	// No record is ever written for it, which is what makes it an orphan.
	name := "u." + guidHex(guid)
	if rerr := os.Rename(filepath.Join(dir, temp), filepath.Join(dir, name)); rerr != nil {
		t.Fatal(rerr)
	}
	when := time.Now().Add(-age)
	if terr := os.Chtimes(filepath.Join(dir, name), when, when); terr != nil {
		t.Fatal(terr)
	}
	return mb, idx, folder, dir, name
}

// A save still in flight is left where it is: its caller names it within the
// cycle, and taking it would take a live message (#1736).
func TestASaveInFlightKeepsItsFile(t *testing.T) {
	mb, _, folder, dir, name := orphanFolder(t, "in flight\n", time.Minute)

	mb.(interface{ SweepTemps(string) }).SweepTemps(folder.Name)

	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Errorf("the file of a save in flight was taken: %v", err)
	}
}

// And one old enough to be a crash's leftover is taken.
func TestAStaleTempIsSwept(t *testing.T) {
	mb, _, folder, dir, name := orphanFolder(t, "left behind\n", 48*time.Hour)

	mb.(interface{ SweepTemps(string) }).SweepTemps(folder.Name)

	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Errorf("a stale temp survived the sweep: %v", err)
	}
}
