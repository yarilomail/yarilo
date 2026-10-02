package file

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A base rewritten between a view's image and its log open pairs the old base
// with the next base's empty log; that read is not an answer (#2078).
func TestAViewDoesNotPairABaseWithTheNextBasesLog(t *testing.T) {
	u := openIdx(t.TempDir(), testUser)
	t.Cleanup(func() { _ = u.Close() })
	f, err := u.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		if aerr := u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i), Size: 10}); aerr != nil {
			t.Fatal(aerr)
		}
	}
	fired := false
	afterViewImage = func() {
		if fired {
			return
		}
		fired = true
		if oerr := u.OptimizeIndex(f.ID); oerr != nil {
			t.Errorf("optimize: %v", oerr)
		}
	}
	t.Cleanup(func() { afterViewImage = nil })

	msgs, err := u.GetMessages(f.ID, mailbox.SeqSet{})
	if !fired {
		t.Fatal("the base was never rewritten under the view, so this row proves nothing")
	}
	if err != nil {
		t.Fatalf("read across the rewrite: %v", err)
	}
	if len(msgs) != 20 {
		t.Errorf("a read across a base rewrite answered %d records, want 20", len(msgs))
	}
}

// A base that keeps moving past the retries is read under the lock, not
// answered with an error or a set built on a base that is already gone.
func TestABaseThatKeepsMovingIsReadUnderTheLock(t *testing.T) {
	u := openIdx(t.TempDir(), testUser)
	t.Cleanup(func() { _ = u.Close() })
	f, err := u.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		if aerr := u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i), Size: 10}); aerr != nil {
			t.Fatal(aerr)
		}
	}
	rewrites := 0
	afterViewImage = func() {
		rewrites++
		if oerr := u.OptimizeIndex(f.ID); oerr != nil {
			t.Errorf("optimize: %v", oerr)
		}
	}
	t.Cleanup(func() { afterViewImage = nil })

	msgs, err := u.GetMessages(f.ID, mailbox.SeqSet{})
	if rewrites <= baseReadAttempts {
		t.Fatalf("the base moved %d times, not past the retries, so this row proves nothing", rewrites)
	}
	if err != nil {
		t.Fatalf("a read under a moving base failed instead of taking the lock: %v", err)
	}
	if len(msgs) != 20 {
		t.Errorf("a read under a moving base answered %d records, want 20", len(msgs))
	}
}
