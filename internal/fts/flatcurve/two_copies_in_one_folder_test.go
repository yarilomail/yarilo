//go:build flatcurve

package flatcurve

import "testing"

// Without a per-copy term the folder is named once however many copies of the
// message it holds: the retraction of one must not take the folder with it.
func TestTwoCopiesInOneFolderKeepTheFolderSearchable(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	guid := testGUID(1)
	indexCopy(t, ui, inbox, 11, guid, nil, []string{"alpha"})
	indexCopy(t, ui, inbox, 12, guid, nil, []string{"alpha"})

	// One copy goes; the store still holds the other one in this folder.
	if err := ui.Expunge(inbox, guid, true, true); err != nil {
		t.Fatal(err)
	}
	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if got := uidsOf(append(res.DefiniteGUIDs, res.MaybeGUIDs...)); len(got) != 1 {
		t.Errorf("INBOX answers %v after one of two copies went, want the message it still holds", got)
	}
}

// The last copy in a folder takes the folder term, and only that: the message
// stays searchable wherever else it lives.
func TestTheLastCopyInAFolderLeavesTheOther(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	guid := testGUID(1)
	indexCopy(t, ui, inbox, 11, guid, nil, []string{"needle"})
	indexCopy(t, ui, archive, 21, guid, nil, []string{"needle"})

	if err := ui.Expunge(inbox, guid, false, true); err != nil {
		t.Fatal(err)
	}
	if got := uidsOf(hitsIn(t, ui, inbox)); len(got) != 0 {
		t.Errorf("INBOX answers %v for a message it no longer holds", got)
	}
	if got := uidsOf(hitsIn(t, ui, archive)); len(got) != 1 {
		t.Errorf("Archive answers %v, want the copy it still holds", got)
	}
}
