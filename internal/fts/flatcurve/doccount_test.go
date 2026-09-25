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
