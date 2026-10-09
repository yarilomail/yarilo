package dboxv2

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// sweepFolder is an sdbox INBOX with its index, and the dbox-Mails leaf the
// sweep reads.
func sweepFolder(t *testing.T) (mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder, string) {
	t.Helper()
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	return mb, idx, folder, filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
}

// save leaves a body under its temp name, as a save does until its uid exists.
func save(t *testing.T, mb mailbox.UserMailbox, body string) (string, [16]byte) {
	t.Helper()
	temp, _, guid, err := mb.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(temp, temporaryPrefix) {
		t.Fatalf("a save is named %q, not a temp: the rows below test nothing", temp)
	}
	return temp, guid
}

func age(t *testing.T, path string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(-d)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func sweep(mb mailbox.UserMailbox, folder string) {
	mb.(interface{ SweepTemps(string) }).SweepTemps(folder)
}

// A save still in flight is left where it is: its caller names it within the
// cycle, and taking it would take a live message (#1736).
func TestASaveInFlightKeepsItsFile(t *testing.T) {
	mb, _, folder, dir := sweepFolder(t)
	temp, _ := save(t, mb, "in flight\n")
	age(t, filepath.Join(dir, temp), time.Minute)

	sweep(mb, folder.Name)

	if _, err := os.Stat(filepath.Join(dir, temp)); err != nil {
		t.Errorf("the file of a save in flight was taken: %v", err)
	}
}

// And one old enough to be a crash's leftover is taken.
func TestAStaleTempIsSwept(t *testing.T) {
	mb, _, folder, dir := sweepFolder(t)
	temp, _ := save(t, mb, "left behind\n")
	age(t, filepath.Join(dir, temp), 48*time.Hour)

	sweep(mb, folder.Name)

	if _, err := os.Stat(filepath.Join(dir, temp)); !os.IsNotExist(err) {
		t.Errorf("a stale temp survived the sweep: %v", err)
	}
}

// A message file is never the sweep's: it cannot see the index, so an old
// u.* with no record is not told apart from a delivered one (#2172).
func TestAnOldMessageWithoutARecordSurvivesTheSweep(t *testing.T) {
	mb, _, folder, dir := sweepFolder(t)
	temp, guid := save(t, mb, "no record\n")
	name := sdboxMailPrefix + guidHex(guid)
	if err := os.Rename(filepath.Join(dir, temp), filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	age(t, filepath.Join(dir, name), 48*time.Hour)

	sweep(mb, folder.Name)

	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Errorf("the sweep took %s: %v", name, err)
	}
}

// A delivered message older than a day survives the folder open that runs the
// sweep, and still reads: the path that emptied sdbox folders on 2.4.1.
func TestAnOldDeliveredMessageSurvivesTheSweepAndReads(t *testing.T) {
	mb, idx, folder, dir := sweepFolder(t)
	const body = "From: a@b\r\n\r\ndelivered a while ago\r\n"
	temp, guid := save(t, mb, body)
	m := &mailbox.MessageMeta{Size: uint32(len(body)), GUID: guid}
	if err := mailboxbase.RecordSaved(idx, mb, folder.ID, "INBOX", temp, m); err != nil {
		t.Fatal(err)
	}
	name, err := mailboxbase.MessagePath(mb, "INBOX", m)
	if err != nil || !strings.HasPrefix(name, sdboxMailPrefix) {
		t.Fatalf("the record names %q (%v), not a message file", name, err)
	}
	age(t, filepath.Join(dir, name), 48*time.Hour)

	box := mailboxbase.Open(mb, idx)
	if _, err := box.Folder("INBOX", folder.UIDValidity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, mailbox.SweepStampName)); err != nil {
		t.Fatalf("the open ran no sweep, so the row tests nothing: %v", err)
	}
	rc, err := box.OpenMessage("INBOX", m)
	if err != nil {
		t.Fatalf("the sweep took the delivered %s: %v", name, err)
	}
	defer rc.Close() //nolint:errcheck
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("read %q, want %q", got, body)
	}
}
