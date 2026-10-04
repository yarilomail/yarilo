package file

import (
	"os"
	"os/exec"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The second process: in one process the shared base-image cache stands
// between two handles, so the seam is only reachable from another.
func TestGUIDStoreWriterHelper(t *testing.T) {
	home := os.Getenv("YARILO_TEST_GUID_HOME")
	if home == "" {
		t.Skip("helper: run by TestAReaderSeesACopyAnotherIndexAppended")
	}
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	defer ui.Close() //nolint:errcheck
	uid := uint32(2041)
	guid := [16]byte{1}
	if os.Getenv("YARILO_TEST_GUID_SECOND") != "" {
		uid, guid = 2042, [16]byte{2}
	}
	if err := ui.applyGUIDBatch(guidBatch{add: []mailbox.GUIDRecord{
		{GUID: guid, FolderGUID: [16]byte{7}, UID: uid},
	}}); err != nil {
		t.Fatal(err)
	}
}

// appendFromAnotherProcess runs the helper and fails the row on its output.
func appendFromAnotherProcess(t *testing.T, home string, second bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestGUIDStoreWriterHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "YARILO_TEST_GUID_HOME="+home)
	if second {
		cmd.Env = append(cmd.Env, "YARILO_TEST_GUID_SECOND=1")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the writing process failed: %v\n%s", err, out)
	}
}

// A reader that only reads sees a copy another process appended: the fts
// service is that reader, resolving every search hit through the store.
func TestAReaderSeesACopyAnotherIndexAppended(t *testing.T) {
	home := t.TempDir()
	open := func() *userIndex {
		return New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	}
	reader := open()
	t.Cleanup(func() { reader.Close() }) //nolint:errcheck

	// Warm before the write: a handle opened afterwards reads from scratch.
	appendFromAnotherProcess(t, home, false)
	if _, err := reader.GUIDCopies([][16]byte{{1}}); err != nil {
		t.Fatal(err)
	}

	appendFromAnotherProcess(t, home, true)
	sent := mailbox.GUIDRecord{GUID: [16]byte{2}, FolderGUID: [16]byte{7}, UID: 2042}
	got, err := reader.GUIDCopies([][16]byte{sent.GUID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UID != sent.UID {
		t.Errorf("the reader answers %v for the copy just appended, want uid %d: its view is one write behind",
			got, sent.UID)
	}
}

// A warm writer appending after another process keeps that copy: it rewrites
// the base from its own records (#2056).
func TestAWarmWriterKeepsAnotherProcessesCopy(t *testing.T) {
	home := t.TempDir()
	mine := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	t.Cleanup(func() { mine.Close() }) //nolint:errcheck

	warm := mailbox.GUIDRecord{GUID: [16]byte{9}, FolderGUID: [16]byte{7}, UID: 2040}
	if err := mine.applyGUIDBatch(guidBatch{add: []mailbox.GUIDRecord{warm}}); err != nil {
		t.Fatal(err)
	}
	appendFromAnotherProcess(t, home, true) // the delivery's copy, uid 2042

	later := mailbox.GUIDRecord{GUID: [16]byte{3}, FolderGUID: [16]byte{7}, UID: 2043}
	if err := mine.applyGUIDBatch(guidBatch{add: []mailbox.GUIDRecord{later}}); err != nil {
		t.Fatal(err)
	}
	got, err := mine.GUIDCopies([][16]byte{{2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UID != 2042 {
		t.Errorf("after this index appended, the other process's copy reads %v, want uid 2042", got)
	}
}
