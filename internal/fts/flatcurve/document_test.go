//go:build flatcurve

package flatcurve

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

var archive = fts.MailboxRef{GUID: "g2", Name: "Archive", UIDValidity: 1}

// shardOf opens the one shard the index holds, for reading a document back.
func shardOf(t *testing.T, ui fts.UserIndex) (*xapianShard, func()) {
	t.Helper()
	// The writer holds the current shard, so it is closed before a row reads
	// the document back; the next call reopens it.
	if err := ui.Close(); err != nil {
		t.Fatal(err)
	}
	st := ui.(*userIndex).state()
	paths, err := shardPaths(st.dir)
	if err != nil || len(paths) == 0 {
		t.Fatalf("no shard to read: %v", err)
	}
	w, err := openShard(paths[len(paths)-1])
	if err != nil {
		t.Fatal(err)
	}
	return &xapianShard{w}, func() { w.Close() }
}

type xapianShard struct {
	w interface {
		DocIDsByTerm(string) ([]uint32, error)
		DocTerms(uint32, string) ([]string, error)
	}
}

// Row 1: a message filed in two folders is one document with both folders and
// both copies on it -- the body is parsed once, not once per copy (#1986).
func TestAMessageInTwoFoldersIsOneDocument(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	guid := testGUID(7)
	indexCopy(t, ui, inbox, 7, guid, nil, []string{"needle"})
	indexCopy(t, ui, archive, 9, guid, nil, []string{"needle"})

	sh, done := shardOf(t, ui)
	defer done()
	ids, err := sh.w.DocIDsByTerm(guidTerm(guid))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("the message has %d documents, want one", len(ids))
	}
	folders, err := sh.w.DocTerms(ids[0], termFolder)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 {
		t.Errorf("the document names %v, want both folders", folders)
	}
	copies, err := sh.w.DocTerms(ids[0], termCopy)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 2 {
		t.Errorf("the document carries %v, want both copies", copies)
	}
}

// Row 2: an expunge takes the copy's terms off the document, and the document
// goes with the last copy.
func TestAnExpungeRemovesTheCopyThenTheDocument(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	guid := testGUID(7)
	indexCopy(t, ui, inbox, 7, guid, nil, []string{"needle"})
	indexCopy(t, ui, archive, 9, guid, nil, []string{"needle"})

	if err := ui.Expunge(inbox, 7); err != nil {
		t.Fatal(err)
	}
	if got := hitsIn(t, ui, inbox); len(got) != 0 {
		t.Errorf("the expunged folder still answers %v", got)
	}
	if got := hitsIn(t, ui, archive); len(got) != 1 {
		t.Fatalf("the other folder's copy went with it: %v", got)
	}
	sh, done := shardOf(t, ui)
	ids, err := sh.w.DocIDsByTerm(guidTerm(guid))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Errorf("the message has %d documents after one copy went, want one", len(ids))
	}
	terms, err := sh.w.DocTerms(ids[0], termFolder)
	if err != nil {
		t.Fatal(err)
	}
	if len(terms) != 1 || terms[0] != folderTerm(archive.GUID) {
		t.Errorf("the document names %v, want the folder that still holds it", terms)
	}
	done()

	if err := ui.Expunge(archive, 9); err != nil {
		t.Fatal(err)
	}
	sh, done = shardOf(t, ui)
	defer done()
	ids, err = sh.w.DocIDsByTerm(guidTerm(guid))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("the document outlived its last copy: %v", ids)
	}
}

// Row 3: a uid is unique in a folder, not in the account. Two messages with
// uid 5 in two folders are two documents, and expunging one leaves the other.
func TestTwoFoldersMayShareAUID(t *testing.T) {
	// Both directions: taking the copy of the folder indexed first and of the
	// one indexed second, because a term without the folder in it is masked by
	// whichever document the lookup happens to reach first.
	for _, tc := range []struct {
		name              string
		expunge, survives fts.MailboxRef
	}{
		{"the folder indexed first", inbox, archive},
		{"the folder indexed second", archive, inbox},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ui, _ := testEngine(t, Options{})
			indexCopy(t, ui, inbox, 5, testGUID(100), nil, []string{"needle"})
			indexCopy(t, ui, archive, 5, testGUID(200), nil, []string{"needle"})
			if got := hitsIn(t, ui, inbox); len(got) != 1 {
				t.Fatalf("INBOX answers %v, want its own message", got)
			}
			if got := hitsIn(t, ui, archive); len(got) != 1 {
				t.Fatalf("Archive answers %v, want its own message", got)
			}

			if err := ui.Expunge(tc.expunge, 5); err != nil {
				t.Fatal(err)
			}
			if got := hitsIn(t, ui, tc.expunge); len(got) != 0 {
				t.Errorf("the expunged copy still answers: %v", got)
			}
			if got := hitsIn(t, ui, tc.survives); len(got) != 1 {
				t.Errorf("expunging (%s,5) took (%s,5) with it: %v",
					tc.expunge.Name, tc.survives.Name, got)
			}
		})
	}
}

// Row 5: a search names the folders it searches; one folder answers only its
// own messages, and the whole account answers both.
func TestASearchAnswersOnlyTheFoldersAsked(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexCopy(t, ui, inbox, 1, testGUID(1), nil, []string{"needle"})
	indexCopy(t, ui, archive, 2, testGUID(2), nil, []string{"needle"})

	if got := hitsIn(t, ui, inbox); len(got) != 1 || got[0] != testGUID(1) {
		t.Errorf("INBOX answers %v, want its own message alone", got)
	}
	if got := hitsIn(t, ui, archive); len(got) != 1 || got[0] != testGUID(2) {
		t.Errorf("Archive answers %v, want its own message alone", got)
	}
	res, err := ui.Lookup(nil, bodyQuery("needle"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.DefiniteGUIDs) != 2 {
		t.Errorf("the account answers %v, want both messages", uidsOf(res.DefiniteGUIDs))
	}
}

func hitsIn(t *testing.T, ui fts.UserIndex, mbox fts.MailboxRef) [][16]byte {
	t.Helper()
	res, err := ui.Lookup([]string{mbox.GUID}, bodyQuery("needle"))
	if err != nil {
		t.Fatal(err)
	}
	return res.DefiniteGUIDs
}
