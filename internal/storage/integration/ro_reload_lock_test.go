package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The #647 hazard, asserted as the property rather than the lock that used to
// provide it: a read cannot poison state it never writes (#1809).
func TestAReadCannotPoisonWhatItNeverWrites(t *testing.T) {
	sock := holdsTestSocket(t)
	clientA := newHoldsClient(t, sock)
	clientB := newHoldsClient(t, sock)

	const username = "dave@example.com"
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: username, Home: home}
	idxA := file.New(file.WithLocker(clientA)).OpenUser(user)
	t.Cleanup(func() { _ = idxA.Close() })

	folder, err := idxA.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	uid, err := idxA.AllocateUID(folder.ID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := idxA.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Client B (another pod) grabs the folder's X lock, standing in for an
	// in-progress compaction holding it.
	key := locks.MailboxKey(username, "INBOX")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	held, err := clientB.Lock(locks.WithSite(ctx, "write"), key, "podB/0/"+username+"/sess2", 30*time.Second)
	if err != nil {
		t.Fatalf("client B lock: %v", err)
	}
	if mode, ok := clientB.HoldsResource(key); !ok || mode != locks.HoldExclusive {
		t.Fatalf("client B holds %q/%v after an exclusive Lock", mode, ok)
	}

	// A read runs through while B holds the key: it writes nothing another
	// session reads, so there is nothing to serialise against (#647, #1809).
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, e := idxA.GetMessages(folder.ID, mailbox.SeqSet{})
		done <- e
	}()
	<-started

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read under another client's hold: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a read waited on a lock it does not take")
	}

	// And the shared state the holder will write from is untouched by it.
	if _, herr := idxA.GetMessages(folder.ID, mailbox.SeqSet{}); herr != nil {
		t.Fatalf("second read: %v", herr)
	}

	if err := clientB.Unlock(ctx, held.ID); err != nil {
		t.Fatalf("client B unlock: %v", err)
	}
}
