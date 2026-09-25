//go:build flatcurve

package flatcurve

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

func copiesOf(uids ...uint32) []fts.Copy {
	out := make([]fts.Copy, 0, len(uids))
	for _, uid := range uids {
		out = append(out, fts.Copy{UID: uid, GUID: testGUID(uid)})
	}
	return out
}

// A rescan reconciles by the message, not by a number the database owns: with
// docids and uids apart, comparing them drops live messages (#2019).
func TestRescanKeepsLiveMessagesWhenDocIDsDiffer(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	// The folder's uids start above the docids the database hands out, which
	// is the ordinary state of a mailbox that has seen deletions.
	indexDoc(t, ui, 11, nil, []string{"bravo"})
	indexDoc(t, ui, 12, nil, []string{"charlie"})

	missing, err := ui.Rescan(inbox, copiesOf(11, 12))
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("rescan reports %v missing, but both are indexed", missing)
	}
	for _, tc := range []struct {
		word string
		uid  uint32
	}{{"bravo", 11}, {"charlie", 12}} {
		res, lerr := ui.Lookup([]string{inbox.GUID}, bodyQuery(tc.word))
		if lerr != nil {
			t.Fatal(lerr)
		}
		if got := uidsOf(append(res.DefiniteGUIDs, res.MaybeGUIDs...)); len(got) != 1 || got[0] != tc.uid {
			t.Errorf("after the rescan %q answers %v, want [%d]", tc.word, got, tc.uid)
		}
	}
}

// A rescan of one folder leaves another folder's documents alone: they share
// one index now, and a walk by number does not know the difference (#2019).
func TestRescanOfOneFolderLeavesTheOtherSearchable(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	other := fts.MailboxRef{GUID: "g2", Name: "Archive", UIDValidity: 1}
	indexDoc(t, ui, 1, nil, []string{"alpha"})
	indexDocIn(t, ui, other, 2, nil, []string{"bravo"})

	if _, err := ui.Rescan(inbox, copiesOf(1)); err != nil {
		t.Fatal(err)
	}
	res, err := ui.Lookup([]string{other.GUID}, bodyQuery("bravo"))
	if err != nil {
		t.Fatal(err)
	}
	if got := uidsOf(append(res.DefiniteGUIDs, res.MaybeGUIDs...)); len(got) != 1 {
		t.Errorf("the other folder answers %v after a rescan of INBOX, want its message", got)
	}
}

// An empty live set is a folder that holds nothing, not an account that does:
// the reset of one folder must not empty the index of the others.
func TestRescanWithAnEmptyFolderKeepsTheRest(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	other := fts.MailboxRef{GUID: "g2", Name: "Archive", UIDValidity: 1}
	indexDoc(t, ui, 1, nil, []string{"alpha"})
	indexDocIn(t, ui, other, 2, nil, []string{"bravo"})

	if _, err := ui.Rescan(inbox, nil); err != nil {
		t.Fatal(err)
	}
	res, err := ui.Lookup([]string{other.GUID}, bodyQuery("bravo"))
	if err != nil {
		t.Fatal(err)
	}
	if got := uidsOf(append(res.DefiniteGUIDs, res.MaybeGUIDs...)); len(got) != 1 {
		t.Errorf("emptying INBOX took the other folder's message with it: %v", got)
	}
}
