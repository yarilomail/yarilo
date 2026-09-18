package file

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// openTestFolder gives a folder with n messages in it, and the state behind it.
func openTestFolder(t *testing.T, n int) (*userIndex, *folderState, *mailbox.Folder) {
	t.Helper()
	u, ok := New().OpenUser(&mailbox.UserInfo{Username: "u1@d00001.test", Home: t.TempDir()}).(*userHandle)
	if !ok {
		t.Fatal("the index no longer hands out a user handle")
	}
	folder, err := u.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		uid, aerr := u.AllocateUID(folder.ID)
		if aerr != nil {
			t.Fatal(aerr)
		}
		if aerr := u.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); aerr != nil {
			t.Fatal(aerr)
		}
	}
	u.ui.mu.Lock()
	fs := u.ui.open[folder.ID]
	u.ui.mu.Unlock()
	if fs == nil {
		t.Fatal("the folder is not open after being opened")
	}
	return u.ui, fs, folder
}

// Every reader of one folder version reads one image: the fold is paid once,
// not once per reader (#1875).
func TestManyViewsOfOneFolderShareOneImage(t *testing.T) {
	_, fs, _ := openTestFolder(t, 20)

	ResetMapCounters()
	const readers = 8
	views := make([]*folderState, readers)
	releases := make([]func(), readers)
	for i := 0; i < readers; i++ {
		v, rel, err := fs.openView()
		if err != nil {
			t.Fatal(err)
		}
		views[i], releases[i] = v, rel
	}
	for i := 1; i < readers; i++ {
		if views[i] != views[0] {
			t.Errorf("reader %d built its own image instead of reading the one that is there", i)
		}
	}
	fs.mapMu.Lock()
	refs := fs.current.refs
	fs.mapMu.Unlock()
	if refs != readers {
		t.Errorf("%d views are open and the image counts %d", readers, refs)
	}
	if got := Folds(); got != 1 {
		t.Errorf("%d readers paid %d folds", readers, got)
	}
	for _, rel := range releases {
		rel()
	}
	fs.mapMu.Lock()
	refs = fs.current.refs
	fs.mapMu.Unlock()
	if refs != 0 {
		t.Errorf("every view is closed and the image still counts %d", refs)
	}
}

// A view opened before a fold keeps answering from the image it took; the fold
// goes onto a copy, and a view opened after sees the new one.
func TestAHeldViewKeepsItsImageAcrossAFold(t *testing.T) {
	u, fs, folder := openTestFolder(t, 5)

	held, release, err := fs.openView()
	if err != nil {
		t.Fatal(err)
	}
	before := len(held.file.Records)

	uid, err := u.AllocateUID(folder.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
		t.Fatal(err)
	}

	after, release2, err := fs.openView()
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if len(held.file.Records) != before {
		t.Errorf("the held view grew from %d to %d records under its reader", before, len(held.file.Records))
	}
	if len(after.file.Records) != before+1 {
		t.Errorf("the view opened after the append has %d records, want %d", len(after.file.Records), before+1)
	}
	release()
}

// With nobody holding the image, a fold copies no records; with a holder, it
// copies once -- the reference's move-to-private, and the only copy left.
func TestAFoldCopiesOnlyWhatSomebodyHolds(t *testing.T) {
	u, fs, folder := openTestFolder(t, 5)

	grow := func() {
		t.Helper()
		uid, err := u.AllocateUID(folder.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := u.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
			t.Fatal(err)
		}
	}

	v, rel, err := fs.openView()
	if err != nil {
		t.Fatal(err)
	}
	_ = v
	rel() // nobody holds it now

	grow()
	ResetMapCounters()
	_, rel2, err := fs.openView()
	if err != nil {
		t.Fatal(err)
	}
	if got := MapCopies(); got != 0 {
		t.Errorf("a fold with no view held copied %d times", got)
	}

	grow()
	_, rel3, err := fs.openView() // rel2 still held: this one must copy
	if err != nil {
		t.Fatal(err)
	}
	if got := MapCopies(); got != 1 {
		t.Errorf("a fold under a held view copied %d times, want 1", got)
	}
	rel2()
	rel3()
}

// Two readers and one fold at once: every view is whole, and the work is done
// once rather than by each of them.
func TestConcurrentOpensPayForOneFold(t *testing.T) {
	u, fs, folder := openTestFolder(t, 30)

	uid, err := u.AllocateUID(folder.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
		t.Fatal(err)
	}

	ResetMapCounters()
	const readers = 16
	var wg sync.WaitGroup
	counts := make([]int, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, rel, verr := fs.openView()
			if verr != nil {
				t.Error(verr)
				return
			}
			counts[i] = len(v.file.Records)
			rel()
		}(i)
	}
	wg.Wait()
	for i, c := range counts {
		if c != 31 {
			t.Errorf("reader %d saw %d records, want 31: it read a half-folded image", i, c)
		}
	}
	if got := Folds(); got != 1 {
		t.Errorf("%d concurrent readers paid %d folds", readers, got)
	}
}

// A compaction replaces the base; the image built on the old one is not reused
// for it, whatever its size and timestamps say.
func TestANewBaseIsNotReadThroughTheOldImage(t *testing.T) {
	u, fs, folder := openTestFolder(t, 4)

	v1, rel1, err := fs.openView()
	if err != nil {
		t.Fatal(err)
	}
	rel1()
	before := len(v1.file.Records)

	uid, err := u.AllocateUID(folder.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
		t.Fatal(err)
	}
	// Fold the log into a fresh base, which is what a compaction does.
	fs.mu.Lock()
	ferr := fs.flush()
	fs.mu.Unlock()
	if ferr != nil {
		t.Fatal(ferr)
	}

	v2, rel2, err := fs.openView()
	if err != nil {
		t.Fatal(err)
	}
	defer rel2()
	if len(v2.file.Records) != before+1 {
		t.Errorf("after the base was rewritten the read has %d records, want %d (path %s)",
			len(v2.file.Records), before+1, filepath.Base(fs.indexPath))
	}
}
