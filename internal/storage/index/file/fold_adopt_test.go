package file

import (
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A process holding what another folds into the base takes it on its next
// locked reload without decoding it again (#1228), and sees what follows.
func TestAReaderAdoptsABaseThatFoldsWhatItHolds(t *testing.T) {
	dial := lockstest.Server(t)
	home := t.TempDir()
	writer, reader := process(t, home, dial()), process(t, home, dial())
	fw := inbox(t, writer)
	for uid := uint32(1); uid <= 3; uid++ {
		if err := writer.AppendMessage(fw.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
			t.Fatal(err)
		}
	}
	fr := inbox(t, reader)
	refresh := reader.(mailbox.FolderRefresher).RefreshFolder
	if err := refresh(fr.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(allOf(t, reader, fr.ID)); n != 3 {
		t.Fatalf("reader holds %d before the fold, want 3", n)
	}

	if err := writer.OptimizeIndex(fw.ID); err != nil {
		t.Fatal(err)
	}
	if err := writer.AppendMessage(fw.ID, &mailbox.MessageMeta{UID: 4, Size: 10, Flags: []string{`\Seen`}}); err != nil {
		t.Fatal(err)
	}
	adopted := testutil.ToFloat64(metricReload.WithLabelValues("adopt"))
	if err := refresh(fr.ID); err != nil {
		t.Fatal(err)
	}
	msgs := allOf(t, reader, fr.ID)
	if got := testutil.ToFloat64(metricReload.WithLabelValues("adopt")); got != adopted+1 {
		t.Errorf("adopt reloads went %v -> %v, want one", adopted, got)
	}
	if len(msgs) != 4 {
		t.Fatalf("reader holds %d after the fold and an append, want 4", len(msgs))
	}
	if !slices.Contains(msgs[3].Flags, `\Seen`) {
		t.Errorf("flags of the append after the adopt: %v", msgs[3].Flags)
	}
}

func allOf(t *testing.T, idx mailbox.UserIndex, folderID uint64) []*mailbox.MessageMeta {
	t.Helper()
	msgs, err := idx.GetMessages(folderID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// adoptedFold leaves reader holding, by an adopt, a base the folder process
// folded after changing a record: what an adopt has to carry is in its headers.
func adoptedFold(t *testing.T, change func(mailbox.UserIndex, uint64) error) (folder, reader mailbox.UserIndex, fFolder, fReader uint64) {
	t.Helper()
	dial := lockstest.Server(t)
	home := t.TempDir()
	first := process(t, home, dial())
	f := inbox(t, first)
	for uid := uint32(1); uid <= 3; uid++ {
		must(t, first.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}))
	}
	must(t, first.ExpungeMessage(f.ID, 1))
	must(t, first.OptimizeIndex(f.ID))
	// Both decode the folded base, so both hold the records as the disk has them.
	folder, reader = process(t, home, dial()), process(t, home, dial())
	fFolder, fReader = inbox(t, folder).ID, inbox(t, reader).ID
	refresh := func(idx mailbox.UserIndex, id uint64) { must(t, idx.(mailbox.FolderRefresher).RefreshFolder(id)) }
	refresh(folder, fFolder)
	refresh(reader, fReader)
	must(t, change(folder, fFolder))
	refresh(reader, fReader)
	must(t, folder.OptimizeIndex(fFolder))
	before := testutil.ToFloat64(metricReload.WithLabelValues("adopt"))
	refresh(reader, fReader)
	if got := testutil.ToFloat64(metricReload.WithLabelValues("adopt")); got != before+1 {
		t.Fatalf("the reader did not adopt the fold (%v -> %v)", before, got)
	}
	return folder, reader, fFolder, fReader
}

// A fold raises the expunge floor; a process that adopted it answers with the
// new floor, or a client syncing from between the two is told nothing vanished.
func TestAnAdoptedFoldCarriesItsExpungeFloor(t *testing.T) {
	folder, reader, ff, fr := adoptedFold(t, func(idx mailbox.UserIndex, id uint64) error {
		return idx.ExpungeMessage(id, 2)
	})
	want, err := folder.ExpungeFloor(ff)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ExpungeFloor(fr)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("the adopting process answers floor %d, the folder's is %d", got, want)
	}
}

// A base the adopting process writes next takes a lineage above the one it
// adopted: a reused number pairs a log with offsets from another file (#1228).
func TestAFlushAfterAnAdoptMintsANewLineage(t *testing.T) {
	_, reader, _, fr := adoptedFold(t, func(idx mailbox.UserIndex, id uint64) error {
		return idx.UpdateFlags(id, 3, []string{`\Seen`}, nil)
	})
	path := reader.(*userHandle).ui.open[fr].indexPath
	adopted, err := peekLineage(path)
	if err != nil {
		t.Fatal(err)
	}
	must(t, reader.OptimizeIndex(fr))
	next, err := peekLineage(path)
	if err != nil {
		t.Fatal(err)
	}
	if next.Lineage <= adopted.Lineage {
		t.Errorf("the next base took lineage %d after adopting %d", next.Lineage, adopted.Lineage)
	}
}
