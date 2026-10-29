package mdbox

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// damage is one way the second of two records in m.1 goes bad on disk.
var damage = map[string]func(d []byte, rec, bodyEnd int){
	"size not a number": func(d []byte, rec, _ int) { copy(d[rec+13:rec+29], "zzzzzzzzzzzzzzzz") },
	"body past the end": func(d []byte, rec, _ int) { copy(d[rec+13:rec+29], "000000000fffffff") },
	"trailer magic":     func(d []byte, _, bodyEnd int) { d[bodyEnd+1] = 'X' },
}

// twoRecordsDamaged saves two messages in m.1, then damages the second; the
// first record's bytes are untouched.
func twoRecordsDamaged(t *testing.T, u *userMailbox, how string, receivedAt time.Time) (first, second string) {
	t.Helper()
	first = saveReceivedAt(t, u, "Subject: one\r\n\r\nfirst body\r\n", receivedAt)
	second = saveReceivedAt(t, u, "Subject: two\r\n\r\nsecond body\r\n", receivedAt)
	path := u.mfilePath(1)
	d := mustRead(t, path)
	starts := recordStarts(t, d)
	if len(starts) != 2 {
		t.Fatalf("setup wants two records in m.1, have %d", len(starts))
	}
	rec := starts[1]
	hdr := bytes.IndexByte(d[rec:], '\n') + 1
	size, err := parseHeaderSize(d[rec : rec+hdr])
	if err != nil {
		t.Fatal(err)
	}
	damage[how](d, rec, rec+hdr+int(size))
	if err := os.WriteFile(path, d, 0o600); err != nil {
		t.Fatal(err)
	}
	return first, second
}

// A scan over a damaged record reports the store as corrupt and keeps the good
// record before it, for every way the record can be damaged.
func TestAScanKeepsTheRecordsBeforeTheDamage(t *testing.T) {
	for how := range damage {
		t.Run(how, func(t *testing.T) {
			u := openTestUserMailbox(t, t.TempDir())
			twoRecordsDamaged(t, u, how, time.Now())
			recs, err := u.scanMFileAt(u.mfilePath(1))
			if !errors.Is(err, errScanCorrupt) {
				t.Errorf("err %v, want a corrupt-record error", err)
			}
			if len(recs) != 1 {
				t.Errorf("kept %d records, want the one before the damage", len(recs))
			}
			if _, err := u.Scan(""); err == nil {
				t.Error("the storage-wide scan reads as complete over a damaged file")
			}
		})
	}
}

// An alt move over a damaged file moves nothing and leaves the good message
// where it was.
func TestAnAltMoveOverADamagedFileMovesNothing(t *testing.T) {
	for how := range damage {
		t.Run(how, func(t *testing.T) {
			_, u, _ := altTestBackend(t)
			first, _ := twoRecordsDamaged(t, u, how, time.Now().Add(-48*time.Hour))
			if _, err := u.AltMove(AltMoveQuery{Before: time.Now()}); err == nil {
				t.Error("alt move over a damaged file succeeded")
			}
			if got := mfiles(t, u.altStoragePath()); len(got) != 0 {
				t.Errorf("alt m-files %v after a failed move", got)
			}
			if b := testFetchBody(t, u, first); !strings.Contains(b, "first body") {
				t.Errorf("good message after the failed move: %q", b)
			}
		})
	}
}

// A purge that cannot read a live record keeps the file whole and leaves no
// half-written copy behind, which no map entry would name.
func TestAPurgeThatCannotReadALiveRecordKeepsTheFile(t *testing.T) {
	u := openTestUserMailbox(t, t.TempDir())
	first, _ := twoRecordsDamaged(t, u, "body past the end", time.Now())
	if err := u.Remove("INBOX", first); err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, u.mfilePath(1))

	if _, err := u.Purge(); err == nil {
		t.Fatal("purge over an unreadable live record succeeded")
	}
	if !bytes.Equal(mustRead(t, u.mfilePath(1)), before) {
		t.Error("m.1 changed under a failed purge")
	}
	if got := mfiles(t, u.storagePath()); len(got) != 1 {
		t.Errorf("m-files %v after a failed purge, want m.1 alone", got)
	}
}

// Writes the disk refuses fail the verb and leave the store as it was.
func TestWritesTheDiskRefusesLeaveTheStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file modes")
	}
	t.Run("save into a read-only m-file", func(t *testing.T) {
		u := openTestUserMailbox(t, t.TempDir())
		kept := testSaveMsg(t, u, "Subject: k\r\n\r\nkept\r\n")
		if err := os.Chmod(u.mfilePath(1), 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(u.mfilePath(1), 0o600) })
		if _, _, _, err := u.Save("INBOX", strings.NewReader("x\r\n"), 0, 3, nil, nil, [16]byte{}); err == nil {
			t.Error("save into a read-only m-file succeeded")
		}
		if b := testFetchBody(t, u, kept); !strings.Contains(b, "kept") {
			t.Errorf("earlier message after the refused save: %q", b)
		}
	})
	t.Run("purge in a read-only storage dir", func(t *testing.T) {
		u := openTestUserMailbox(t, t.TempDir())
		gone := testSaveMsg(t, u, "Subject: g\r\n\r\ngone\r\n")
		live := testSaveMsg(t, u, "Subject: l\r\n\r\nlive\r\n")
		if err := u.Remove("INBOX", gone); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(u.storagePath(), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(u.storagePath(), 0o700) })
		if _, err := u.Purge(); err == nil {
			t.Error("purge with no room for the compacted file succeeded")
		}
		if b := testFetchBody(t, u, live); !strings.Contains(b, "live") {
			t.Errorf("live message after the refused purge: %q", b)
		}
	})
	t.Run("alt move into a read-only alt dir", func(t *testing.T) {
		_, u, _ := altTestBackend(t)
		name := saveReceivedAt(t, u, "Subject: o\r\n\r\nold\r\n", time.Now().Add(-48*time.Hour))
		if err := os.MkdirAll(u.altStoragePath(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(u.altStoragePath(), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(u.altStoragePath(), 0o700) })
		if _, err := u.AltMove(AltMoveQuery{Before: time.Now()}); err == nil {
			t.Error("alt move into a read-only alt dir succeeded")
		}
		if b := testFetchBody(t, u, name); !strings.Contains(b, "old") {
			t.Errorf("message after the refused alt move: %q", b)
		}
	})
}

// A split writes its alt half first: when the primary half cannot be written,
// the alt half goes too, since no map entry names it for a purge to reclaim.
func TestASplitThatCannotWriteItsPrimaryHalfLeavesNoAltCopy(t *testing.T) {
	_, u, _ := altTestBackend(t)
	old := saveReceivedAt(t, u, "Subject: o\r\n\r\nold\r\n", time.Now().Add(-48*time.Hour))
	fresh := saveReceivedAt(t, u, "Subject: n\r\n\r\nnew\r\n", time.Now())
	// The split takes m.2 for alt and m.3 for primary; a directory at m.3
	// refuses the primary half and nothing else.
	if err := os.Mkdir(u.mfilePath(3), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := u.AltMove(AltMoveQuery{Before: time.Now().Add(-time.Hour)}); err == nil {
		t.Fatal("split that cannot write its primary half succeeded")
	}
	if got := mfiles(t, u.altStoragePath()); len(got) != 0 {
		t.Errorf("alt m-files %v left by the failed split", got)
	}
	for name, want := range map[string]string{old: "old", fresh: "new"} {
		if b := testFetchBody(t, u, name); !strings.Contains(b, want) {
			t.Errorf("%s after the failed split: %q", want, b)
		}
	}
}

// A damaged record header reads as corruption, not as a missing message; a
// damaged trailer costs the body nothing, its length is in the header.
func TestAFetchOfADamagedRecordIsCorruption(t *testing.T) {
	for how := range damage {
		t.Run(how, func(t *testing.T) {
			u := openTestUserMailbox(t, t.TempDir())
			first, second := twoRecordsDamaged(t, u, how, time.Now())
			rc, err := u.Fetch("INBOX", second, false)
			var body []byte
			if err == nil {
				body, err = io.ReadAll(rc)
				_ = rc.Close()
			}
			switch {
			case how == "trailer magic":
				if err != nil || !strings.Contains(string(body), "second body") {
					t.Errorf("fetch past a damaged trailer: %q, %v", body, err)
				}
			case !errors.Is(err, mailbox.ErrCorruptStorage):
				t.Errorf("fetch of the damaged record: %v, want corruption", err)
			}
			if b := testFetchBody(t, u, first); !strings.Contains(b, "first body") {
				t.Errorf("the good record beside it: %q", b)
			}
		})
	}
}

// A name that parses but that the map does not hold is corruption to every
// verb that reads it, not a message to invent.
func TestAMapUIDTheMapDoesNotHoldIsCorruption(t *testing.T) {
	u := openTestUserMailbox(t, t.TempDir())
	testSaveMsg(t, u, "Subject: x\r\n\r\nx\r\n")
	if err := u.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := u.Move("INBOX", "Archive", "4000", [16]byte{}); !errors.Is(err, mailbox.ErrCorruptStorage) {
		t.Errorf("move of map_uid 4000: %v, want corruption", err)
	}
}
