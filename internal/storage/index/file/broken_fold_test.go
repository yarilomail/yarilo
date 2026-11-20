package file

import (
	"errors"
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A fold that stops part way onto a held view's writer, followed by a rebuild
// that fails once, leaves no half-applied map in service: the next read
// answers what the base and the whole journal say (#2627 review).
func TestAFoldThatStopsPartWayIsNeverExtendedAgain(t *testing.T) {
	home := movedFolder(t, 300)
	w, wf := acceptOpen(t, home)
	defer w.Close() //nolint:errcheck
	r, rf := acceptOpen(t, home)
	defer r.Close() //nolint:errcheck

	// A first change makes the current map one with a writer of its own.
	if err := w.UpdateFlags(wf.ID, 3, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetMessages(rf.ID, mailbox.SeqSet{}); err != nil {
		t.Fatal(err)
	}
	fs, err := r.(*userHandle).ui.state(rf.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, hold, err := fs.openView() // a reader keeps the map current
	if err != nil {
		t.Fatal(err)
	}
	defer hold()

	for i := 0; i < 3; i++ {
		uid, err := w.AllocateUID(wf.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.AppendMessage(wf.ID, &mailbox.MessageMeta{UID: uid, Size: 10, Flags: []string{`\Seen`}}); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := w.Begin(wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Expunge(7)
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	stopped := errors.New("the replay stopped")
	records := 0
	replaySeam = func() error {
		if records++; records == 2 {
			return stopped
		}
		return nil
	}
	rebuilds := 0
	buildViewSeam = func() error {
		if rebuilds++; rebuilds == 1 {
			return stopped
		}
		return nil
	}
	_, _, ferr := fs.openView()
	replaySeam, buildViewSeam = nil, nil
	if !errors.Is(ferr, stopped) {
		t.Fatalf("the fold went through: %v; the seams did not fire", ferr)
	}

	fresh, ff := acceptOpen(t, home)
	defer fresh.Close() //nolint:errcheck
	want, err := fresh.GetMessages(ff.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.GetMessages(rf.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after the stopped fold the reader answers %d messages, a fresh open %d", len(got), len(want))
	}
	again, err := r.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if again.Messages != ff.Messages || again.Unseen != ff.Unseen || again.NextUID != ff.NextUID {
		t.Errorf("the reader's folder says %d messages, %d unseen, next %d; a fresh open %d, %d, %d",
			again.Messages, again.Unseen, again.NextUID, ff.Messages, ff.Unseen, ff.NextUID)
	}
}
