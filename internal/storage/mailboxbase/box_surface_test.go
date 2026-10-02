package mailboxbase_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-message/textproto"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// surfaceBox is one maildir account with two delivered messages, through Box.
func surfaceBox(t *testing.T) (*mailboxbase.Box, mailbox.UserIndex, string, *mailbox.Folder) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@example.com", Home: home, Driver: "maildir"}
	store := maildir.New().OpenUser(info)
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	cur := filepath.Join(home, "Maildir", "cur")
	for _, n := range []string{"1700000001.M1P1.h:2,", "1700000002.M2P2.h:2,S"} {
		if err := os.WriteFile(filepath.Join(cur, n), []byte("Subject: x\r\n\r\nbody\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	box := mailboxbase.Open(store, idx)
	f, err := box.Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	return box, idx, home, f
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// Every method Box gained for the protocol servers reaches the index it stands
// for (#1805).
func TestTheBoxSurfaceReachesTheIndex(t *testing.T) {
	t.Run("Vanished names an expunged uid", func(t *testing.T) {
		box, idx, _, f := surfaceBox(t)
		if err := idx.ExpungeMessage(f.ID, 1); err != nil {
			t.Fatal(err)
		}
		got, err := box.Vanished(f.ID, 1)
		if err != nil || len(got) != 1 || got[0] != 1 {
			t.Errorf("vanished %v, %v; want [1]", got, err)
		}
	})
	t.Run("Keywords lists a keyword set on a record", func(t *testing.T) {
		box, _, _, f := surfaceBox(t)
		if err := box.UpdateFlags(f.ID, 1, mailbox.FlagsUpdate{Mode: mailbox.FlagsAdd, Keywords: []string{"$Work"}}); err != nil {
			t.Fatal(err)
		}
		got, err := box.Keywords(f.ID)
		if err != nil || len(got) == 0 || got[len(got)-1] != "$Work" {
			t.Errorf("keywords %v, %v; want $Work among them", got, err)
		}
	})
	t.Run("Metadata counts the folder", func(t *testing.T) {
		box, _, _, f := surfaceBox(t)
		md, err := box.Metadata(f.ID)
		if err != nil || md.Messages != 2 || md.VSize == 0 {
			t.Errorf("metadata %+v, %v; want 2 messages and a size", md, err)
		}
	})
	t.Run("CreateFolder writes the index, Rename and Delete carry it", func(t *testing.T) {
		box, idx, _, _ := surfaceBox(t)
		dirOf := idx.(interface{ IndexDirFor(string) string }).IndexDirFor
		if err := box.Store().Create("Work"); err != nil {
			t.Fatal(err)
		}
		box.CreateFolder("Work", 7)
		if !exists(filepath.Join(dirOf("Work"), "yarilo.index")) {
			t.Fatal("CreateFolder wrote no index")
		}
		if err := box.Rename("Work", "Play"); err != nil || !exists(filepath.Join(dirOf("Play"), "yarilo.index")) {
			t.Fatalf("Rename did not carry the index: %v", err)
		}
		if err := box.Delete("Play"); err != nil || exists(dirOf("Play")) {
			t.Errorf("Delete left the index: %v", err)
		}
	})
	t.Run("RebuildFolder restores a lost index from the store", func(t *testing.T) {
		_, idx, home, _ := surfaceBox(t)
		dir := idx.(interface{ IndexDirFor(string) string }).IndexDirFor("INBOX")
		lost, _ := filepath.Glob(filepath.Join(dir, "yarilo.index*"))
		if len(lost) == 0 {
			t.Fatal("no index files to lose, so this row proves nothing")
		}
		for _, p := range lost {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
		// A new process: nothing of the lost index is held in memory.
		info := &mailbox.UserInfo{Username: "u@example.com", Home: home, Driver: "maildir"}
		store := maildir.New().OpenUser(info)
		fresh := file.New().OpenUser(info)
		t.Cleanup(func() { _ = store.Close(); _ = fresh.Close() })
		box := mailboxbase.Open(store, fresh, mailboxbase.ReadOnly())
		f, err := box.Folder("INBOX", 0)
		if err != nil {
			t.Fatal(err)
		}
		n, err := box.RebuildFolder(f)
		if err != nil || n != 2 {
			t.Errorf("rebuilt %d, %v; want the 2 messages in the store", n, err)
		}
	})
	t.Run("VanishedGUIDs names an expunge by its message, and says when it cannot", func(t *testing.T) {
		box, idx, _, f := surfaceBox(t)
		if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 9, Size: 10}); err != nil {
			t.Fatal(err)
		}
		msgs, err := box.Messages(f.ID, mailbox.SeqSet{{From: 1, To: 1}})
		if err != nil || len(msgs) != 1 {
			t.Fatal(err)
		}
		if err := idx.ExpungeMessage(f.ID, 1); err != nil {
			t.Fatal(err)
		}
		guids, complete, err := box.VanishedGUIDs(f.ID, 1)
		if err != nil || !complete || len(guids) != 1 || guids[0] != msgs[0].GUID {
			t.Fatalf("vanished %x complete %v, %v; want uid 1's message, complete", guids, complete, err)
		}
		if err := idx.ExpungeMessage(f.ID, 9); err != nil {
			t.Fatal(err)
		}
		if _, complete, err := box.VanishedGUIDs(f.ID, 1); err != nil || complete {
			t.Errorf("an expunge of a record with no message id answered complete %v, %v", complete, err)
		}
	})
	t.Run("FolderStamp holds while nothing moves and moves with a write", func(t *testing.T) {
		box, idx, _, f := surfaceBox(t)
		a, err := box.FolderStamp("INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if b, err := box.FolderStamp("INBOX"); err != nil || b != a {
			t.Fatalf("the stamp moved with nothing written: %v", err)
		}
		if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 9, Size: 10}); err != nil {
			t.Fatal(err)
		}
		if c, err := box.FolderStamp("INBOX"); err != nil || c == a {
			t.Errorf("the stamp held across a write: %v", err)
		}
	})
	t.Run("EnvelopeCache keeps what a window stored", func(t *testing.T) {
		box, _, _, f := surfaceBox(t)
		msgs, err := box.Messages(f.ID, mailbox.SeqSet{})
		if err != nil || len(msgs) == 0 {
			t.Fatal(err)
		}
		c := box.EnvelopeCache(f.ID, mailbox.EnvelopeCacheOptions{})
		var h textproto.Header
		h.Set("Subject", "kept")
		c.StoreFromHeader(msgs[0], h, `(NIL "kept" NIL NIL NIL NIL NIL NIL NIL NIL)`)
		c.StoreSentDate(msgs[0], time.Unix(1700000000, 0))
		c.Close()
		msgs, _ = box.Messages(f.ID, mailbox.SeqSet{})
		c = box.EnvelopeCache(f.ID, mailbox.EnvelopeCacheOptions{})
		defer c.Close()
		if text, ok := c.EnvelopeText(msgs[0]); !ok || text == "" {
			t.Errorf("the cache lost the envelope it was given: %q %v", text, ok)
		}
	})
}
