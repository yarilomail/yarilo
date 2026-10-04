package imap_test

import (
	"strconv"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
)

// The client is told in the same order as before: the untagged EXPUNGEs come
// highest sequence first, and all of them before the tagged OK (#1853).
func TestTheExpungeOrderTheClientSeesIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	c := startQuotaWarnServer(t, dir, maildir.New(), true)

	for i := 0; i < 3; i++ {
		msg := "From: s@x\r\nTo: user@test.com\r\nSubject: m" + strconv.Itoa(i) + "\r\n\r\nbody\r\n"
		ac := c.Append("INBOX", int64(len(msg)), nil)
		if _, err := ac.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		if err := ac.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := ac.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(imap.SeqSetNum(1, 2, 3),
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}
	seqs, err := c.Expunge().Collect()
	if err != nil {
		t.Fatalf("expunge: %v", err)
	}

	if len(seqs) != 3 {
		t.Fatalf("the client saw %d untagged EXPUNGEs, want 3: %v", len(seqs), seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] > seqs[i-1] {
			t.Errorf("the sequence numbers arrived %v: a later one is higher, so the client renumbers wrongly", seqs)
			break
		}
	}
}
