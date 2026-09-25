package imap_test

import "testing"

// DELETE retracts the mailbox from the search index: with one index per user
// its documents outlive the folder otherwise, and no per-folder pass sees
// them again (#2022).
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

	fake.mu.Lock()
	got := append([]string(nil), fake.droppedFolders...)
	fake.mu.Unlock()
	if len(got) != 1 || got[0] != "Archive" {
		t.Fatalf("DELETE retracted %v, want [Archive]: its documents stay in the index otherwise", got)
	}
}
