package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	mailboxbase "github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A full pass records what it read the uid list at, and an arrivals-only pass
// after it opens the window from that stamp (#1875).
func TestAStampedUIDListOpensTheWindow(t *testing.T) {
	box, idx, folder := recSetup(t)
	deliverToNew(t, box, "1700000001.M1Pa.host", "body\r\n")
	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	stamped, ok := idx.(mailbox.MaildirStamped)
	if !ok {
		t.Fatal("the index keeps no maildir stamp")
	}
	if _, have := stamped.MaildirStamp(folder.ID); !have {
		t.Fatal("a full pass recorded no stamp, so the next open has nothing to ask")
	}

	box.folderCacheFor("INBOX").invalidateUIDs("test")
	hit := testutil.ToFloat64(metricStampHit)
	if _, err := box.ReconcileArrivals(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	if now := testutil.ToFloat64(metricStampHit); now != hit+1 {
		t.Errorf("stamp hits = %v, want %v", now, hit+1)
	}
	if !windowOpen(box, "INBOX") {
		t.Error("the stamp matched and the window is still shut")
	}
}

// Somebody else's append moves the list, and then the stamp says nothing: the
// window stays shut and the list is read as before.
func TestAForeignAppendClosesTheStamp(t *testing.T) {
	box, idx, folder := recSetup(t)
	deliverToNew(t, box, "1700000001.M1Pa.host", "body\r\n")
	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	// Written by hand, the way another session's append reaches this one.
	list := box.uidListPath("INBOX")
	body, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(list, append(body, []byte("\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	box.folderCacheFor("INBOX").invalidateUIDs("test")
	miss := testutil.ToFloat64(metricStampMiss)
	if _, err := box.ReconcileArrivals(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	if now := testutil.ToFloat64(metricStampMiss); now != miss+1 {
		t.Errorf("stamp misses = %v, want %v", now, miss+1)
	}
	if windowOpen(box, "INBOX") {
		t.Error("the list moved and the window opened anyway")
	}
}

// Inside the window the stamp opened, a name lookup asks the list no questions.
func TestAStampedWindowCostsNoListStat(t *testing.T) {
	box, idx, folder := recSetup(t)
	deliverToNew(t, box, "1700000001.M1Pa.host", "body\r\n")
	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	if _, err := box.ReconcileArrivals(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, err := box.List("INBOX")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("list = %v, err = %v", msgs, err)
	}
	box.folderCacheFor("INBOX").markChecked()

	stats := testutil.ToFloat64(metricCacheStat.WithLabelValues("list"))
	for i := 0; i < 3; i++ {
		if _, err := box.RecordPath("INBOX", msgs[0]); err != nil {
			t.Fatal(err)
		}
	}
	if now := testutil.ToFloat64(metricCacheStat.WithLabelValues("list")); now != stats {
		t.Errorf("three lookups stated the list %v times inside the window, want 0", now-stats)
	}
}

// The stamp is the reference's own header, so its fields land where that
// install reads them (maildir-storage.h:52-56).
func TestTheStampSitsInTheReferenceLayout(t *testing.T) {
	box, idx, folder := recSetup(t)
	deliverToNew(t, box, "1700000001.M1Pa.host", "body\r\n")
	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	// The index, wherever this layout puts it: a row that skips when it cannot
	// find the file proves nothing.
	var raw []byte
	err := filepath.WalkDir(box.home, func(path string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || d.Name() != "yarilo.index" {
			return werr
		}
		var rerr error
		raw, rerr = os.ReadFile(path)
		return rerr
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("no index file was written, so this row would have passed on nothing")
	}
	if !strings.Contains(string(raw), "maildir") {
		t.Error("the index carries no extension named maildir, so that install reads none")
	}
	stamped := idx.(mailbox.MaildirStamped)
	got, _ := stamped.MaildirStamp(folder.ID)
	if got.UIDListSize == 0 || got.UIDListMtime == 0 {
		t.Errorf("the stamp says size=%d mtime=%d, which names no file", got.UIDListSize, got.UIDListMtime)
	}
}

// windowOpen reads the window itself: snapshotChecked also wants a map loaded,
// which is a different question from whether the window is open.
func windowOpen(u *userMailbox, folder string) bool {
	c := u.folderCacheFor(folder)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checked
}

// A list rewritten in the same second to the same size differs only in the
// nanoseconds, which is the ordinary case (maildir-sync-index.c:255-257).
func TestTheStampPinsTheNanoseconds(t *testing.T) {
	box, idx, folder := recSetup(t)
	deliverToNew(t, box, "1700000001.M1Pa.host", "body\r\n")
	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	list := box.uidListPath("INBOX")
	fi, err := statPath(list)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	// The same bytes, the same second, a different nanosecond: size and
	// seconds alone cannot tell this rewrite from no rewrite at all.
	if err := os.WriteFile(list, body, 0o600); err != nil {
		t.Fatal(err)
	}
	same := time.Unix(fi.ModTime().Unix(), int64(fi.ModTime().Nanosecond())+1000)
	if err := os.Chtimes(list, same, same); err != nil {
		t.Fatal(err)
	}

	// The window the full pass earned is shut first, or this row would read
	// that one rather than the stamp.
	box.folderCacheFor("INBOX").invalidateUIDs("test")
	miss := testutil.ToFloat64(metricStampMiss)
	if _, err := box.ReconcileArrivals(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	if now := testutil.ToFloat64(metricStampMiss); now != miss+1 {
		t.Errorf("stamp misses = %v, want %v: the rewrite went unnoticed", now, miss+1)
	}
	if windowOpen(box, "INBOX") {
		t.Error("a list rewritten inside the second opened the window anyway")
	}
}
