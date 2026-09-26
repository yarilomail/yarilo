package file

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A base replaced while it is being read must not label the content read with
// the identity of the file that replaced it: every later read then matches the
// cache and answers from a version that is already gone (#2056).
func TestAnImageCarriesTheIdentityOfWhatItRead(t *testing.T) {
	home := t.TempDir()
	reader := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	t.Cleanup(func() { reader.Close() }) //nolint:errcheck

	appendFromAnotherProcess(t, home, false) // uid 2041
	if _, err := reader.GUIDCopies([][16]byte{{1}}); err != nil {
		t.Fatal(err)
	}

	// The next read finds the file changed and parses it; while it does, the
	// other process appends again, as a delivery does.
	appendFromAnotherProcess(t, home, false) // uid 2041 again: the cache goes stale
	once := false
	afterBaseRead = func() {
		if once {
			return
		}
		once = true
		appendFromAnotherProcess(t, home, true) // the delivery's copy, uid 2042
	}
	t.Cleanup(func() { afterBaseRead = nil })
	if _, err := reader.GUIDCopies([][16]byte{{1}}); err != nil {
		t.Fatal(err)
	}

	afterBaseRead = nil
	got, err := reader.GUIDCopies([][16]byte{{2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UID != 2042 {
		t.Errorf("the reader answers %v, want uid 2042: its image was labelled with a file it never read", got)
	}
}

// The sweep asks for every indexed message at once and drops what the store
// does not know. A copy another process appended must read as present, or a
// delivered message's document is swept as dead (#2031 class).
func TestASweepSizedLookupSeesACopyAppendedDuringARead(t *testing.T) {
	home := t.TempDir()
	reader := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	t.Cleanup(func() { reader.Close() }) //nolint:errcheck

	appendFromAnotherProcess(t, home, false)
	reader.GUIDCopies([][16]byte{{1}}) //nolint:errcheck
	appendFromAnotherProcess(t, home, false)
	once := false
	afterBaseRead = func() {
		if once {
			return
		}
		once = true
		appendFromAnotherProcess(t, home, true)
	}
	t.Cleanup(func() { afterBaseRead = nil })
	reader.GUIDCopies([][16]byte{{1}}) //nolint:errcheck
	afterBaseRead = nil

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
			t.Errorf("the sweep reads %x as dead, though its copy is in the store", g)
		}
	}
}

// A writer the reader cannot outrun: the retries are spent, and the content is
// still labelled with the file it was read from, so the reader catches up on
// its next read instead of holding a version that is gone.
func TestAReadThatSpentItsRetriesCatchesUp(t *testing.T) {
	home := t.TempDir()
	reader := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	t.Cleanup(func() { reader.Close() }) //nolint:errcheck

	appendFromAnotherProcess(t, home, false)
	reader.GUIDCopies([][16]byte{{1}}) //nolint:errcheck
	appendFromAnotherProcess(t, home, false)

	// One write per read, exactly until the retries run out. The last of them
	// is the delivery's copy, so only the version the read ends on holds it.
	left := baseReadAttempts + 1
	afterBaseRead = func() {
		if left == 0 {
			return
		}
		left--
		appendFromAnotherProcess(t, home, left == 0)
	}
	t.Cleanup(func() { afterBaseRead = nil })
	reader.GUIDCopies([][16]byte{{1}}) //nolint:errcheck
	afterBaseRead = nil

	got, err := reader.GUIDCopies([][16]byte{{2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UID != 2042 {
		t.Errorf("the reader answers %v once the writes stopped, want uid 2042: it kept a version that is gone", got)
	}
}
