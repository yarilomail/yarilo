package file

import (
	"errors"
	"os"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// goneUnderOpenState opens Work with two messages here, then deletes it from a
// second index over the same root, as another process would.
func goneUnderOpenState(t *testing.T) (*userIndex, *mailbox.Folder, string) {
	t.Helper()
	root := t.TempDir()
	u := openIdx(root, testUser)
	t.Cleanup(func() { _ = u.Close() })
	f, err := u.OpenFolder("Work", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= 2; uid++ {
		if err := u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	other := openIdx(root, testUser)
	defer other.Close() //nolint:errcheck
	if err := other.DeleteFolder("Work"); err != nil {
		t.Fatal(err)
	}
	dir := u.indexDir("Work")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the other process left %s, so this row proves nothing: %v", dir, err)
	}
	return u, f, dir
}

func evicted(u *userIndex, id uint64) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, open := u.open[id]
	return !open
}

func TestAReadOfAFolderDeletedElsewhereSaysItIsGone(t *testing.T) {
	for _, read := range []struct {
		name string
		get  func(*userIndex, uint64) ([]*mailbox.MessageMeta, error)
	}{
		{"read", func(u *userIndex, id uint64) ([]*mailbox.MessageMeta, error) { return u.GetMessages(id, nil) }},
		{"refresh", func(u *userIndex, id uint64) ([]*mailbox.MessageMeta, error) { return nil, u.RefreshFolder(id) }},
		{"locked fallback", func(u *userIndex, id uint64) ([]*mailbox.MessageMeta, error) {
			// An index from before the lineage extension is read under the lock.
			u.mu.Lock()
			fs := u.open[id]
			u.mu.Unlock()
			fs.mu.Lock()
			fs.lineage = lineageHdr{Lineage: lineageUnknown}
			fs.mu.Unlock()
			return u.GetMessages(id, nil)
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			u, f, _ := goneUnderOpenState(t)
			msgs, err := read.get(u, f.ID)
			if !errors.Is(err, mailbox.ErrFolderGone) {
				t.Fatalf("read served %d messages of a deleted folder, err %v; want ErrFolderGone", len(msgs), err)
			}
			if !evicted(u, f.ID) {
				t.Error("the deleted folder's state is still open")
			}
		})
	}
}

func TestAWriteToAFolderDeletedElsewhereDoesNotBringItBack(t *testing.T) {
	u, f, dir := goneUnderOpenState(t)
	err := u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 3})
	if !errors.Is(err, mailbox.ErrFolderGone) {
		t.Errorf("write to a deleted folder: err %v, want ErrFolderGone", err)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("the write made %s again: %v", dir, serr)
	}
}

func TestAFolderDeletedElsewhereReopensNew(t *testing.T) {
	u, f, _ := goneUnderOpenState(t)
	_, _ = u.GetMessages(f.ID, nil)
	again, err := u.OpenFolder("Work", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.NextUID != 1 || again.GUID == f.GUID || again.UIDValidity == f.UIDValidity {
		t.Errorf("reopened Work: NextUID %d, guid %x→%x, uidvalidity %d→%d; want 1 and both new",
			again.NextUID, f.GUID, again.GUID, f.UIDValidity, again.UIDValidity)
	}
}

func TestFlushNeverMakesALoadedFoldersDirectoryAgain(t *testing.T) {
	u, f, dir := goneUnderOpenState(t)
	u.mu.Lock()
	fs := u.open[f.ID]
	u.mu.Unlock()
	fs.mu.Lock()
	err := fs.flush()
	fs.mu.Unlock()
	if !errors.Is(err, mailbox.ErrFolderGone) {
		t.Errorf("flush of a deleted folder: err %v, want ErrFolderGone", err)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("flush made %s again: %v", dir, serr)
	}
}

func TestNothingLoadedYetStillTakesAMissingFile(t *testing.T) {
	fs := &folderState{folder: "Work", indexPath: "/nonexistent/yarilo.index"}
	if err := fs.missingBase(os.ErrNotExist); err != nil {
		t.Errorf("a state with nothing loaded refused a missing file: %v", err)
	}
}
