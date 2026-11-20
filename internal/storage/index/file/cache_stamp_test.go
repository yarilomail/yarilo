package file

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// Stamping one message's cache offset, what a FETCH that cached new fields
// does, goes to the journal: it does not rewrite the folder's base (step
// 3b-2b; reviewer, 2026-11-20).
func TestACacheStampDoesNotRewriteTheBase(t *testing.T) {
	home := movedFolder(t, 2000)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	u := h.(*userHandle).ui
	// The extensions are declared by a first stamp; the row is the next one.
	if err := u.SetCacheOffsets(f.ID, map[uint32]mailbox.CacheStamp{1: {Offset: 64, CRC: 1}}); err != nil {
		t.Fatal(err)
	}
	fs, err := u.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	before := fs.flushes
	fs.mu.Unlock()
	if err := u.SetCacheOffsets(f.ID, map[uint32]mailbox.CacheStamp{2: {Offset: 128, CRC: 2}}); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	rewrites := fs.flushes - before
	fs.mu.Unlock()
	if rewrites != 0 {
		t.Errorf("stamping one cache offset rewrote the base %d times, want 0: a FETCH would rewrite all 2000 records", rewrites)
	}
	msgs, err := h.GetMessages(f.ID, mailbox.SeqSet{{From: 2, To: 2}})
	if err != nil || len(msgs) != 1 || msgs[0].CacheOffset != 128 || msgs[0].CacheCRC != 2 {
		t.Fatalf("the stamp reads back as %+v: %v", msgs, err)
	}
}
