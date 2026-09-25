//go:build flatcurve

package flatcurve

import "testing"

// Deleting a mailbox takes its documents with it, but only its own: a message
// that also lives elsewhere stays searchable there (#2022).
func TestDropFolderKeepsAMessageHeldElsewhere(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	shared := testGUID(1)
	indexCopy(t, ui, inbox, 1, shared, nil, []string{"alpha"})
	indexCopy(t, ui, archive, 7, shared, nil, []string{"alpha"})
	indexDocIn(t, ui, archive, 8, nil, []string{"bravo"})

	if err := ui.DropFolder(archive); err != nil {
		t.Fatal(err)
	}
	if n, err := ui.DocCount(); err != nil || n != 1 {
		t.Errorf("DocCount = %d (err %v) after dropping Archive, want 1: only the shared message survives", n, err)
	}
	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if got := uidsOf(append(res.DefiniteGUIDs, res.MaybeGUIDs...)); len(got) != 1 {
		t.Errorf("INBOX answers %v for a message it still holds, want it found", got)
	}
	if res, err = ui.Lookup([]string{archive.GUID}, bodyQuery("alpha")); err != nil {
		t.Fatal(err)
	}
	if got := uidsOf(append(res.DefiniteGUIDs, res.MaybeGUIDs...)); len(got) != 0 {
		t.Errorf("the deleted folder still answers %v", got)
	}
}

// A folder term naming a mailbox the account no longer has is an orphan no
// per-folder pass can reach; the sweep is what clears it (#2022).
func TestDropOrphanFoldersClearsADeletedFoldersDocuments(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 1, nil, []string{"alpha"})
	indexDocIn(t, ui, archive, 7, nil, []string{"bravo"})

	dropped, err := ui.DropOrphanFolders([]string{inbox.GUID})
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Errorf("dropped %d orphan folder terms, want 1", dropped)
	}
	if n, cerr := ui.DocCount(); cerr != nil || n != 1 {
		t.Errorf("DocCount = %d (err %v) after the sweep, want 1", n, cerr)
	}
	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if got := uidsOf(append(res.DefiniteGUIDs, res.MaybeGUIDs...)); len(got) != 1 {
		t.Errorf("the sweep took a live folder's message too: %v", got)
	}
}
