package imap_test

import (
	"testing"
	"time"
)

// DELETE retracts the mailbox from the search index: its documents outlive
// the folder otherwise, and no per-folder pass sees them again (#2022).
func TestDeleteRetractsTheFolderFromFTS(t *testing.T) {
	fake := &fakeFTS{lastUID: 100}
	c := startFTSTestServer(t, fake, true)
	if err := c.Create("Archive", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("Archive", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	appendBody(t, c, "a message that is about to lose its folder")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete("Archive").Wait(); err != nil {
		t.Fatal(err)
	}

	// The retraction runs off the command path, so the row waits for it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		fake.mu.Lock()
		got := append([]string(nil), fake.droppedFolders...)
		fake.mu.Unlock()
		if len(got) == 1 && got[0] == "Archive" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("DELETE retracted %v, want [Archive]: its documents stay in the index otherwise", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
