package file

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// Two parses of one base are one image to the map but two record sets: the next
// fold must not extend the cached one in place, which every handle reads (#2265).
func TestAFoldNeverWritesIntoTheCachedBase(t *testing.T) {
	u, fs, folder := openTestFolder(t, 30)
	if err := u.OptimizeIndex(folder.ID); err != nil {
		t.Fatal(err)
	}
	cached, err := imageFor(fs)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mailindex.Open(fs.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	other := &baseImage{file: parsed, ident: cached.ident, size: cached.size, mod: cached.mod,
		keywords: cached.keywords, lineage: cached.lineage}

	m, err := fs.fold(other, logStatOf(fs.indexPath), nil)
	if err != nil {
		t.Fatal(err)
	}
	fs.mapMu.Lock()
	fs.current = m
	fs.mapMu.Unlock()

	before, count := imageRecords(cached), cached.file.Header.MessagesCount
	uid, err := u.AllocateUID(folder.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
		t.Fatal(err)
	}
	v, rel, err := fs.openView()
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	if got := v.recordCount(); got != 31 {
		t.Errorf("the view after the append holds %d records, want 31", got)
	}
	if imageRecords(cached) != before || cached.file.Header.MessagesCount != count {
		t.Errorf("the fold wrote into the cached base: %d records (count %d), it held %d (%d)",
			imageRecords(cached), cached.file.Header.MessagesCount, before, count)
	}
}

func imageRecords(img *baseImage) int {
	if img.recs != nil {
		return img.recs.Len()
	}
	return len(img.file.Records)
}
