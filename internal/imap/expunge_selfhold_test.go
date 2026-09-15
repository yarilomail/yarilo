package imap_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
)

// dropInNew puts a message under new/ the way a delivery does, so the next
// walk has work and takes the folder hold.
func dropInNew(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "Maildir", "new")
	if _, err := os.Stat(dir); err != nil {
		dir = filepath.Join(home, "INBOX", "new")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "1700000001.M1Pz.host")
	if err := os.WriteFile(name, []byte("From: a@b\r\nSubject: out of band\r\n\r\nbody\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A session that never counted its usage still answers its EXPUNGE: with no
// cached total the post-commit read walks, and a walk re-opens the folder
// (#1853).
func TestAnExpungeWithNoCountedUsageStillAnswers(t *testing.T) {
	dir := t.TempDir()
	c, _, dial := startCloningServerAt(t, dir)
	appendOne(t, c, "doomed")

	// A second session: it has counted nothing, so the post-commit read has no
	// total to adjust and walks every folder instead.
	c = dial(t)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(imap.SeqSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}
	// Something for the walk to find: a folder with nothing to do is skipped,
	// and a skipped walk never asks for the hold.
	dropInNew(t, filepath.Join(dir, "test.com", "user"))

	type result struct {
		seqs []uint32
		err  error
	}
	done := make(chan result, 1)
	go func() {
		seqs, err := c.Expunge().Collect()
		done <- result{seqs, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("expunge: %v", r.err)
		}
		if len(r.seqs) != 1 || r.seqs[0] != 1 {
			t.Errorf("the client was told %v, want one untagged EXPUNGE for sequence 1", r.seqs)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the EXPUNGE never answered: the session waits on the hold it took itself (#1853)")
	}
}
