package mdbox

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func scanDate(t *testing.T, u *userMailbox, guid [16]byte) time.Time {
	t.Helper()
	recs, err := u.Scan("")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.GUID == guid {
			return r.InternalDate
		}
	}
	t.Fatalf("no stored record carries guid %x", guid)
	return time.Time{}
}

// An APPEND date survives the storage rebuild, a purge that rewrites the
// record, and a move that re-saves it: all three read or carry R (#2175).
func TestTheAppendDateSurvivesARebuildAPurgeAndAMove(t *testing.T) {
	u := openTestUserMailbox(t, filepath.Join(t.TempDir(), "home"))
	when := time.Date(2024, 11, 23, 17, 45, 9, 0, time.UTC)
	const gone, kept = "From: a@b\r\n\r\ngone\r\n", "From: a@b\r\n\r\nkept\r\n"
	first, _, _, err := u.SaveReceived("INBOX", strings.NewReader(gone), 0, int64(len(gone)), nil, nil, [16]byte{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	name, _, guid, err := u.SaveReceived("INBOX", strings.NewReader(kept), 0, int64(len(kept)), nil, nil, [16]byte{}, when)
	if err != nil {
		t.Fatal(err)
	}
	if got := scanDate(t, u, guid); !got.Equal(when) {
		t.Errorf("after the save the rebuild reads %s, want %s", got, when)
	}

	if err := u.Remove("INBOX", first); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Purge(); err != nil {
		t.Fatal(err)
	}
	u.mu.Lock()
	u.mapping = nil
	u.mu.Unlock()
	if got := scanDate(t, u, guid); !got.Equal(when) {
		t.Errorf("after the purge the rebuild reads %s, want %s", got, when)
	}

	if err := u.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := u.Move("INBOX", "Archive", name, guid); err != nil {
		t.Fatal(err)
	}
	// The moved-from record stays in its file, same GUID, until a purge.
	if _, err := u.Purge(); err != nil {
		t.Fatal(err)
	}
	u.mu.Lock()
	u.mapping = nil
	u.mu.Unlock()
	if got := scanDate(t, u, guid); !got.Equal(when) {
		t.Errorf("after the move the rebuild reads %s, want %s", got, when)
	}
}
