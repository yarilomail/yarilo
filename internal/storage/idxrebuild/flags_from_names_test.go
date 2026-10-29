package idxrebuild_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/idxrebuild"
	fileidx "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// In maildir the file name is where flags live: a rebuild takes a flag another
// program set by renaming over the index's older set, and keeps the uid.
func TestARebuildTakesTheFlagsAMaildirNameCarries(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: home(root, "u@x.com")}
	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	t.Cleanup(func() { _ = box.Close(); _ = idx.Close() })
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	name, _, guid, err := box.Save("INBOX", strings.NewReader("a\n"), 1, 2, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	publish(t, box, "INBOX", name, 1)
	if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: 1, Size: 2, VSize: 2, GUID: guid, Flags: []string{`\Flagged`}}); err != nil {
		t.Fatal(err)
	}
	// Another program marks it read the maildir way: new/ to cur/ with S.
	dir := filepath.Join(info.Home, "Maildir")
	entries, err := os.ReadDir(filepath.Join(dir, "new"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("new/ holds %d files, %v", len(entries), err)
	}
	old := entries[0].Name()
	base, _, _ := strings.Cut(old, ":2,")
	if err := os.Rename(filepath.Join(dir, "new", old), filepath.Join(dir, "cur", base+":2,S")); err != nil {
		t.Fatal(err)
	}

	folder, _ = idx.OpenFolder("INBOX", 1)
	st, err := idxrebuild.RebuildFolder(mailboxbase.Open(box, idx), idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if st.UIDsPreserved != 1 {
		t.Fatalf("stats %+v, want the uid kept", st)
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 1 || msgs[0].UID != 1 || !slices.Equal(msgs[0].Flags, []string{`\Seen`}) {
		t.Errorf("after the rebuild: %+v, want uid 1 with the name's \\Seen", msgs)
	}
}

// A record from before GUIDs names no message to look for, so the heal that
// drops records whose file vanished leaves it alone.
func TestAHealLeavesARecordWithoutAGUID(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: home(root, "u@x.com")}
	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	t.Cleanup(func() { _ = box.Close(); _ = idx.Close() })
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: 1, Size: 2}); err != nil {
		t.Fatal(err)
	}
	gone, err := idxrebuild.ExpungeMissing(mailboxbase.Open(box, idx), idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 0 {
		t.Errorf("the heal expunged %v", gone)
	}
}
