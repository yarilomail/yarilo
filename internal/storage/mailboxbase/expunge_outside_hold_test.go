package mailboxbase_test

import (
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Nothing the caller does with the result can reach back into the hold: the
// hold is over by the time it has one (#1853).
func TestTheHoldIsOverBeforeTheCallerIsTold(t *testing.T) {
	box, f := openBox(t, "u9@example.com")
	msgs := fillForExpunge(t, box, f, 3)

	removed, failed, err := box.ExpungeMarked(f, "INBOX", msgs)
	if err != nil || failed != 0 {
		t.Fatalf("expunge: err=%v failed=%d", err, failed)
	}
	if len(removed) != 3 {
		t.Fatalf("the call removed %d messages, want 3", len(removed))
	}

	// What the session does next is what deadlocked inside the hold: opening
	// the folder again. Outside it, it answers.
	done := make(chan error, 1)
	go func() {
		_, ferr := box.Folder("INBOX", f.UIDValidity)
		done <- ferr
	}()
	select {
	case ferr := <-done:
		if ferr != nil {
			t.Fatalf("the folder could not be reopened after the expunge: %v", ferr)
		}
	case <-timeoutAfter():
		t.Fatal("reopening the folder after an expunge blocked: the hold outlived the call")
	}

	// And the messages the caller was handed are the ones that went.
	left, lerr := box.Index().GetMessages(f.ID, mailbox.SeqSet{})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(left) != 0 {
		t.Errorf("%d records survived the expunge", len(left))
	}
	for _, m := range removed {
		if m.UID == 0 {
			t.Error("a removed message came back with no uid, so a caller cannot report it")
		}
	}
}

// The hold reaches no folder open: Box.Folder inside it is what re-entered the
// driver's mutex and hung the session (#1853).
func TestTheHoldOpensNoFolder(t *testing.T) {
	box, f := openBox(t, "u10@example.com")
	msgs := fillForExpunge(t, box, f, 3)

	before := folderOpens(t)
	done := make(chan error, 1)
	go func() {
		_, _, err := box.ExpungeMarked(f, "INBOX", msgs)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-timeoutAfter():
		// A folder opened inside the hold asks the driver for the hold it is
		// already inside, and the call never returns (#1853).
		t.Fatal("the expunge did not return: something inside the hold waits on it")
	}
	if got := folderOpens(t) - before; got != 0 {
		t.Errorf("the expunge opened the folder %v times inside its hold, want 0", got)
	}
}

// folderOpens counts every pass through Box.Folder's settle, by either arm.
func folderOpens(t *testing.T) float64 {
	t.Helper()
	total := 0.0
	for _, result := range []string{"scanned", "scanned-untokened", "skipped"} {
		total += syncCount(t, result)
	}
	return total
}

// fillForExpunge puts n messages in the folder and returns their records.
func fillForExpunge(t *testing.T, box *mailboxbase.Box, f *mailbox.Folder, n int) []*mailbox.MessageMeta {
	t.Helper()
	out := make([]*mailbox.MessageMeta, 0, n)
	for i := 0; i < n; i++ {
		body := "From: a@b\r\nSubject: m\r\n\r\nbody\r\n"
		uid, err := box.Index().AllocateUID(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		saved, vsize, guid, serr := box.Store().Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
		if rerr := box.RecordDelivered(f, "INBOX", saved, m); rerr != nil {
			t.Fatal(rerr)
		}
		out = append(out, m)
	}
	return out
}

// timeoutAfter is the "it did not answer" side of both rows.
func timeoutAfter() <-chan time.Time { return time.After(2 * time.Second) }
