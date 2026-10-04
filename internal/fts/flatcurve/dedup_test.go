//go:build flatcurve

package flatcurve

import (
	"testing"

	"github.com/0kaba0hub/go-xapian"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// Insurance, not the mechanism: a copy joins as it is written, so a document
// per copy survives only in an index written before that (#1986).
func TestCompactionFoldsTheCopiesOfOneMessage(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1})
	archive := fts.MailboxRef{GUID: "g2", Name: "Archive", UIDValidity: 1}
	guid := testGUID(7)
	indexCopy(t, ui, inbox, 7, guid, nil, []string{"needle"})
	// A second shard, and in it the copy as the old write path left it: its own
	// document under the same message.
	indexCopy(t, ui, inbox, 8, testGUID(8), nil, []string{"filler"})
	if err := ui.Refresh(); err != nil {
		t.Fatal(err)
	}
	writeStrayCopy(t, ui, guid, archive.GUID, 9)

	merged := testutil.ToFloat64(metricDedupMerged)
	if err := ui.OptimizeMailbox(fts.MailboxRef{}); err != nil {
		t.Fatal(err)
	}
	if now := testutil.ToFloat64(metricDedupMerged); now != merged+1 {
		t.Errorf("the compaction folded %v documents, want one", now-merged)
	}

	st := ui.(*userIndex).state()
	paths, err := shardPaths(st.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("the compaction left %d shards", len(paths))
	}
	// One document, carrying both folders and both copies.
	w, err := openShard(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ids, err := w.DocIDsByTerm(guidTerm(testGUID(7)))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("the message has %d documents after the compaction, want one", len(ids))
	}
	folders, err := w.DocTerms(ids[0], termFolder)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 {
		t.Errorf("the document names %v, want both folders", folders)
	}

	// And both folders still answer, each with its own uid.
	for _, tc := range []struct {
		mbox fts.MailboxRef
		uid  uint32
	}{{inbox, 7}, {archive, 9}} {
		res, lerr := ui.Lookup([]string{tc.mbox.GUID}, bodyQuery("needle"))
		if lerr != nil {
			t.Fatal(lerr)
		}
		if got := res.DefiniteGUIDs; len(got) != 1 {
			t.Errorf("folder %q answers %v, want one message", tc.mbox.Name, got)
		}
	}
}

// A mailbox whose uids are past what a docid can hold still indexes and
// searches: the store answers with the uid, the docid is the database's own.
func TestAMailboxPastTheDocIDCeilingIndexesAndSearches(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	const huge = uint32(1) << 31 // past half of what a docid can count
	for _, uid := range []uint32{huge, huge + 1} {
		indexCopy(t, ui, inbox, uid, testGUID(uid), nil, []string{"needle"})
	}
	if err := ui.Refresh(); err != nil {
		t.Fatal(err)
	}

	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("needle"))
	if err != nil {
		t.Fatal(err)
	}
	if got := uidsOf(res.DefiniteGUIDs); len(got) != 2 || got[0] != huge || got[1] != huge+1 {
		t.Fatalf("the search answers %v, want the two high uids", got)
	}

	// And a retraction names the message, so it finds that one alone.
	if err := ui.Expunge(inbox, testGUID(huge), false, false); err != nil {
		t.Fatal(err)
	}
	res, err = ui.Lookup([]string{inbox.GUID}, bodyQuery("needle"))
	if err != nil {
		t.Fatal(err)
	}
	if got := uidsOf(res.DefiniteGUIDs); len(got) != 1 || got[0] != huge+1 {
		t.Errorf("after expunging %d the search answers %v", huge, got)
	}
}

// writeStrayCopy writes a second document for one message into the newest
// shard, the way the engine did before a copy joined across shards.
func writeStrayCopy(t *testing.T, ui fts.UserIndex, guid [16]byte, folderGUID string, uid uint32) {
	t.Helper()
	st := ui.(*userIndex).state()
	paths, err := shardPaths(st.dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := openShard(paths[len(paths)-1])
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	doc := xapian.NewDoc()
	defer doc.Free()
	for _, term := range []string{guidTerm(guid), folderTerm(folderGUID), "needl"} {
		if err := doc.AddBooleanTerm(term); err != nil {
			t.Fatal(err)
		}
	}
	if err := doc.SetValue(slotGUID, string(guid[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := w.AddDocument(doc); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
}
