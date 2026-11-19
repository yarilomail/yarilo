package file

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The journal twins of the rows in view_label_test.go: another process appends
// to the store's journal and the base stays as it was, which is how a delivery
// writes the store (#2617). A reader whose image ignores the journal keeps a
// version that is gone and misses the copy.

func TestAnImageCarriesTheJournalItRead(t *testing.T) {
	home := t.TempDir()
	reader := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	t.Cleanup(func() { reader.Close() }) //nolint:errcheck

	appendFromAnotherProcess(t, home, false) // uid 2041
	if _, err := reader.GUIDCopies([][16]byte{{1}}); err != nil {
		t.Fatal(err)
	}
	appendFromAnotherProcess(t, home, false) // uid 2041 again
	if _, err := reader.GUIDCopies([][16]byte{{1}}); err != nil {
		t.Fatal(err)
	}
	appendFromAnotherProcess(t, home, true) // the delivery's copy, uid 2042
	got, err := reader.GUIDCopies([][16]byte{{2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UID != 2042 {
		t.Errorf("the reader answers %v, want uid 2042: its image did not take the journal in", got)
	}
}

func TestASweepSizedLookupSeesACopyAppendedToTheJournal(t *testing.T) {
	home := t.TempDir()
	reader := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	t.Cleanup(func() { reader.Close() }) //nolint:errcheck

	appendFromAnotherProcess(t, home, false)
	reader.GUIDCopies([][16]byte{{1}}) //nolint:errcheck
	appendFromAnotherProcess(t, home, false)
	reader.GUIDCopies([][16]byte{{1}}) //nolint:errcheck
	appendFromAnotherProcess(t, home, true)

	indexed := [][16]byte{{1}, {2}}
	recs, err := reader.GUIDCopies(indexed)
	if err != nil {
		t.Fatal(err)
	}
	alive := map[[16]byte]bool{}
	for _, r := range recs {
		alive[r.GUID] = true
	}
	for _, g := range indexed {
		if !alive[g] {
			t.Errorf("the sweep reads %x as dead, though its copy is in the store's journal", g)
		}
	}
}

func TestAReaderCatchesUpWithAJournalThatKeptGrowing(t *testing.T) {
	home := t.TempDir()
	reader := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	t.Cleanup(func() { reader.Close() }) //nolint:errcheck

	appendFromAnotherProcess(t, home, false)
	reader.GUIDCopies([][16]byte{{1}}) //nolint:errcheck
	// One write per read, as many as the base rows retry; the last is the
	// delivery's copy.
	for left := baseReadAttempts + 1; left > 0; left-- {
		appendFromAnotherProcess(t, home, left == 1)
		reader.GUIDCopies([][16]byte{{1}}) //nolint:errcheck
	}
	got, err := reader.GUIDCopies([][16]byte{{2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UID != 2042 {
		t.Errorf("the reader answers %v once the writes stopped, want uid 2042: it kept a version that is gone", got)
	}
}
