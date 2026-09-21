package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// deliver saves a message the way a delivery does -- no flags -- and gives it a
// uid, returning the name it was published under.
func deliver(t *testing.T, box *userMailbox, folder, body string, uid uint32) string {
	t.Helper()
	name, _, _, err := box.Save(folder, strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := box.AssignUID(folder, name, uid); err != nil {
		t.Fatalf("assign uid: %v", err)
	}
	return name
}

// A flagless delivery goes into new/ under a bare name, a flagged one into cur/
// with the flags in its name (maildir-save.c:251-256).
func TestADeliveryWithNoFlagsLandsInTheArrivalDirectory(t *testing.T) {
	box, root := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck
	inbox := box.folderPath("INBOX")
	_ = root

	bare := deliver(t, box, "INBOX", "From: a@b\r\n\r\nplain\r\n", 1)
	if strings.Contains(bare, ":2,") {
		t.Errorf("a flagless delivery was named %q, which carries an info part", bare)
	}
	if _, err := os.Stat(filepath.Join(inbox, "new", bare)); err != nil {
		t.Errorf("the delivery is not in new/: %v", err)
	}

	body := "From: a@b\r\n\r\nflagged\r\n"
	flagged, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), []string{`\Seen`}, nil, [16]byte{})
	if err != nil {
		t.Fatalf("save flagged: %v", err)
	}
	if _, err := box.AssignUID("INBOX", flagged, 2); err != nil {
		t.Fatalf("assign uid: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inbox, "cur", flagged)); err != nil {
		t.Errorf("the flagged save is not in cur/: %v", err)
	}
}

// Every reader resolves a body that is still in new/. One row per reader,
// because four call sites can lose the fallback one at a time (#1959).
func TestEveryReaderFindsABodyStillInTheArrivalDirectory(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck
	name := deliver(t, box, "INBOX", "From: a@b\r\nSubject: s\r\n\r\nbody\r\n", 1)
	if _, err := os.Stat(filepath.Join(box.folderPath("INBOX"), "new", name)); err != nil {
		t.Fatalf("the row needs the file in new/: %v", err)
	}

	t.Run("Fetch opens it", func(t *testing.T) {
		r, err := box.Fetch("INBOX", name, false)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		r.Close() //nolint:errcheck
	})
	t.Run("the record names it", func(t *testing.T) {
		got, err := box.RecordPath("INBOX", &mailbox.MessageMeta{UID: 1})
		if err != nil || got != name {
			t.Errorf("RecordPath = %q, %v; want %q", got, err, name)
		}
	})
	t.Run("its size is measured", func(t *testing.T) {
		// A name our own save writes carries S= and W=, and RecordSize answers
		// from those without touching the disk. The measured path is reached by
		// a name that carries neither -- which is what a foreign MDA leaves in
		// new/, and the case this fallback is for.
		foreign := "1700009000.M1P1.mda.example"
		body := "From: a@b\r\n\r\nforeign\r\n"
		if err := os.WriteFile(filepath.Join(box.folderPath("INBOX"), "new", foreign), []byte(body), 0o600); err != nil {
			t.Fatalf("the MDA's delivery: %v", err)
		}
		if _, err := box.AssignUID("INBOX", foreign, 2); err != nil {
			t.Fatalf("record it: %v", err)
		}
		psize, _, err := box.RecordSize("INBOX", &mailbox.MessageMeta{UID: 2})
		if err != nil || psize != uint32(len(body)) {
			t.Errorf("RecordSize = %d, %v; want %d measured from the file", psize, err, len(body))
		}
	})
	t.Run("a scan counts it as readable", func(t *testing.T) {
		if !box.bodyReadable("INBOX", name) {
			t.Error("the body was judged unreadable while it sat in new/")
		}
	})
}

// Remove unlinks from wherever the file is, and leaves nothing behind in the
// other directory either (#1959).
func TestRemoveTakesAMessageOutOfTheArrivalDirectory(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck
	name := deliver(t, box, "INBOX", "From: a@b\r\n\r\nx\r\n", 1)

	if err := box.Remove("INBOX", name); err != nil {
		t.Fatalf("remove: %v", err)
	}
	for _, sub := range []string{"new", "cur"} {
		if _, err := os.Stat(filepath.Join(box.folderPath("INBOX"), sub, name)); err == nil {
			t.Errorf("the message is still in %s/ after Remove", sub)
		}
	}
}

// The fallback reads one name, not a directory: listing new/ per message is
// what #1809 took out of the expunge hold, and a resolver that lists it again
// puts it straight back.
func TestResolvingAnArrivalReadsNoDirectory(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck
	const n = 5
	for i := 1; i <= n; i++ {
		deliver(t, box, "INBOX", "From: a@b\r\n\r\nx\r\n", uint32(i))
	}

	ResetDirReads()
	for i := 1; i <= n; i++ {
		if _, err := box.RecordPath("INBOX", &mailbox.MessageMeta{UID: uint32(i)}); err != nil {
			t.Fatalf("uid %d: %v", i, err)
		}
	}
	if got := DirReads(); got > 1 {
		t.Errorf("resolving %d arrivals read directories %d times, want at most 1", n, got)
	}
}

// A removal from new/ must not disturb the listing cache, which is cur/'s: it
// never held that file, and dropping it costs a re-read of a directory nobody
// changed (#1809, #1959).
func TestARemovalFromTheArrivalDirectoryKeepsTheListingCache(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck
	body := "From: a@b\r\n\r\nx\r\n"
	flagged, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), []string{`\Seen`}, nil, [16]byte{})
	if err != nil {
		t.Fatalf("save flagged: %v", err)
	}
	if _, err := box.AssignUID("INBOX", flagged, 1); err != nil {
		t.Fatalf("assign uid: %v", err)
	}
	arrival := deliver(t, box, "INBOX", body, 2)
	if _, err := box.List("INBOX"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if box.folderCacheFor("INBOX").entries == nil {
		t.Fatal("the listing cache was not populated, so the row proves nothing")
	}

	if err := box.Remove("INBOX", arrival); err != nil {
		t.Fatalf("remove the arrival: %v", err)
	}
	if box.folderCacheFor("INBOX").entries == nil {
		t.Error("removing a file from new/ dropped the listing cache of cur/")
	}
}
