//go:build flatcurve

package flatcurve

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// A copy is not a second document: the count an operator reads has to tell
// "582 messages" apart from "582 copies of fewer messages" (#2021).
func TestDocCountCountsMessagesNotCopies(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	other := fts.MailboxRef{GUID: "g2", Name: "Archive", UIDValidity: 1}
	guid := testGUID(1)
	indexCopy(t, ui, inbox, 1, guid, nil, []string{"alpha"})
	indexCopy(t, ui, other, 7, guid, nil, []string{"alpha"})

	got, err := ui.DocCount()
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("DocCount = %d for one message in two folders, want 1", got)
	}
}

// A count is a read: with an update's documents written but not committed,
// it refuses rather than commit half of somebody else's batch (#2021).
func TestDocCountRefusesWhileABatchIsInFlight(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	up, err := ui.BeginUpdate(inbox)
	if err != nil {
		t.Fatal(err)
	}
	key := func(uid uint32) fts.BuildKey {
		return fts.BuildKey{UID: uid, GUID: testGUID(uid), Type: fts.KeyBodyPart, ContentType: "text/plain"}
	}
	if ok, serr := up.SetBuildKey(key(1)); !ok || serr != nil {
		t.Fatalf("build key: ok=%v err=%v", ok, serr)
	}
	if err := up.BuildMore([]byte("alpha")); err != nil {
		t.Fatal(err)
	}
	// The second key writes the first document without committing it.
	if ok, serr := up.SetBuildKey(key(2)); !ok || serr != nil {
		t.Fatalf("build key: ok=%v err=%v", ok, serr)
	}

	if n, cerr := ui.DocCount(); cerr == nil {
		t.Errorf("DocCount = %d with a batch in flight, want a refusal instead of a commit", n)
	}
	if err := up.Commit(); err != nil {
		t.Fatal(err)
	}
	n, err := ui.DocCount()
	if err != nil {
		t.Fatalf("DocCount after the batch committed: %v", err)
	}
	if n != 2 {
		t.Errorf("DocCount = %d after the batch committed, want 2", n)
	}
}
