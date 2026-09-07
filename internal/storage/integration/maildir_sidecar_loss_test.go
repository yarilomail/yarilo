package integration_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const oldBody = "From: a@b\r\n\r\nold body\r\n"

// oldMaildirFolder is the shape an older build left: files in cur/, records that
// name them through the sidecar alone, and a uid list that never heard of them.
// withSizes says whether the names carry the ,S= and ,W= fields.
func oldMaildirFolder(t *testing.T, withSizes bool) (string, mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder, []string) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	cur := filepath.Join(home, "Maildir", "cur")
	names := make([]string, 0, 2)
	lines := ""
	for i := 1; i <= 2; i++ {
		base := fmt.Sprintf("170000000%d.M1P1_%d.host", i, i)
		if withSizes {
			base += fmt.Sprintf(",S=%d,W=%d", len(oldBody), len(oldBody))
		}
		name := base + ":2,"
		if err := os.WriteFile(filepath.Join(cur, name), []byte(oldBody), 0o600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		sum := sha256.Sum256([]byte(base))
		var guid [16]byte
		copy(guid[:], sum[:16])
		// Size and vsize zero: the sidecar held both, which is why a record that
		// lost it reads back as an empty message (#1726).
		if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i), GUID: guid}); err != nil {
			t.Fatal(err)
		}
		lines += fmt.Sprintf("%d\t%s\t%d\n", i, name, len(oldBody))
	}
	sidecar := filepath.Join(indexDirOf(t, idx), "yarilo.index.names")
	if err := os.WriteFile(sidecar, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, box, idx, f, names
}

func migrateNames(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, f *mailbox.Folder) int {
	t.Helper()
	mig, ok := mailbox.Driver(box).(interface {
		MigrateUIDNames(mailbox.UserIndex, *mailbox.Folder) (int, error)
	})
	if !ok {
		t.Fatal("the maildir driver does not migrate uid names")
	}
	n, err := mig.MigrateUIDNames(idx, f)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return n
}

// A record named only by the sidecar keeps its file and its size when the
// sidecar goes: removed unread, both were thrown away (#1726).
func TestAMaildirRecordNamedOnlyBySidecarSurvivesTheMigration(t *testing.T) {
	_, box, idx, f, names := oldMaildirFolder(t, true)

	if placed := migrateNames(t, box, idx, f); placed != 2 {
		t.Fatalf("the pass placed %d records, want 2", placed)
	}
	if _, err := os.Stat(filepath.Join(indexDirOf(t, idx), "yarilo.index.names")); !os.IsNotExist(err) {
		t.Errorf("the sidecar is still there after every record it named was placed: %v", err)
	}

	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range msgs {
		got, perr := mailbox.MessagePath(box, "INBOX", m)
		if perr != nil {
			t.Fatalf("uid %d can no longer be named: %v", m.UID, perr)
		}
		if got != names[i] {
			t.Errorf("uid %d names %q, want %q", m.UID, got, names[i])
		}
		size, vsize, serr := mailbox.MessageSize(box, "INBOX", m)
		if serr != nil {
			t.Fatalf("uid %d has no size: %v", m.UID, serr)
		}
		if size != uint32(len(oldBody)) || vsize != uint32(len(oldBody)) {
			t.Errorf("uid %d reads back %d/%d, want %d/%d",
				m.UID, size, vsize, len(oldBody), len(oldBody))
		}
	}
}

// Where the sidecar is already gone -- the folders dev.343 opened -- the guid the
// base name derives is what puts the record back (#1726).
func TestARecordWhoseSidecarIsGoneIsFoundByItsGUID(t *testing.T) {
	_, box, idx, f, names := oldMaildirFolder(t, true)
	if err := os.Remove(filepath.Join(indexDirOf(t, idx), "yarilo.index.names")); err != nil {
		t.Fatal(err)
	}

	if placed := migrateNames(t, box, idx, f); placed != 2 {
		t.Fatalf("the pass placed %d records, want 2", placed)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range msgs {
		got, perr := mailbox.MessagePath(box, "INBOX", m)
		if perr != nil {
			t.Fatalf("uid %d was not recovered: %v", m.UID, perr)
		}
		if got != names[i] {
			t.Errorf("uid %d names %q, want %q", m.UID, got, names[i])
		}
	}
}

// A record the pass cannot place keeps the sidecar in place: the file is the
// only thing that still names it, and the next open has to try again (#1713).
func TestTheSidecarStaysWhileARecordItNamesIsUnplaced(t *testing.T) {
	_, box, idx, f, _ := oldMaildirFolder(t, true)
	// One of the two reaches the list; the other does not.
	defer maildir.SetTestPlaceLimit(1)()

	migrateNames(t, box, idx, f)
	if _, err := os.Stat(filepath.Join(indexDirOf(t, idx), "yarilo.index.names")); err != nil {
		t.Errorf("the sidecar was removed while it still named an unplaced record: %v", err)
	}
	marker, ok := idx.(mailbox.UIDNameMarker)
	if !ok {
		t.Fatal("the index cannot answer for the pass")
	}
	if done, err := marker.UIDNamed(f.ID); err != nil || done {
		t.Errorf("the folder was marked done with a record unplaced (done=%v, err=%v)", done, err)
	}
}

// indexDirOf is where this user's INBOX index lives.
func indexDirOf(t *testing.T, idx mailbox.UserIndex) string {
	t.Helper()
	d, ok := idx.(interface{ IndexDirFor(string) string })
	if !ok {
		t.Fatal("the index cannot say where a folder lives")
	}
	return d.IndexDirFor("INBOX")
}
