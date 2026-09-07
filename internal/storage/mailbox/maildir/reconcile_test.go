package maildir

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func recSetup(t *testing.T) (*userMailbox, mailbox.UserIndex, *mailbox.Folder) {
	t.Helper()
	root := t.TempDir()
	const user = "u@x.com"
	home := testHome(root, user)
	info := &mailbox.UserInfo{Username: user, Home: home}
	box := New().OpenUser(info).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatalf("create: %v", err)
	}
	idx := fileidx.New().OpenUser(info)
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	return box, idx, folder
}

// deliverToNew drops a file into new/ exactly as an external MDA would: a bare
// base name, no ":2," info trailer.
func deliverToNew(t *testing.T, box *userMailbox, name, body string) {
	t.Helper()
	p := filepath.Join(box.folderPath("INBOX"), "new", name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("deliver to new: %v", err)
	}
}

func msgByUID(msgs []*mailbox.MessageMeta, uid uint32) *mailbox.MessageMeta {
	for _, m := range msgs {
		if m.UID == uid {
			return m
		}
	}
	return nil
}

// F1: a message delivered into new/ must be migrated to cur/ and become
// readable via Fetch — the whole point of the feature.
func TestReconcile_ImportsFromNewAndIsFetchable(t *testing.T) {
	box, idx, folder := recSetup(t)
	deliverToNew(t, box, "1700000000.MDA.host", "hello body\n")

	st, err := box.ReconcileIndex(idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Changed || st.Imported != 1 {
		t.Fatalf("stats = %+v, want imported=1 changed", st)
	}

	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 1 || msgs[0].UID != 1 {
		t.Fatalf("index = %+v, want one message UID 1", msgs)
	}
	name := storedName(t, box, "INBOX", msgs[0])
	if !strings.Contains(name, ":2,") {
		t.Fatalf("migrated name %q lacks the :2, info marker", name)
	}
	// new/ must be empty now, cur/ must hold the file, and Fetch must read it.
	rc, err := box.Fetch("INBOX", name, false)
	if err != nil {
		t.Fatalf("fetch migrated message: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello body\n" {
		t.Fatalf("fetched body = %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(box.folderPath("INBOX"), "new")); len(entries) != 0 {
		t.Fatalf("new/ still has %d files after sync", len(entries))
	}
}

// F3: an out-of-band flag change renames the file (":2," trailer) but keeps the
// base name; the message must keep its UID and adopt the new flags.
func TestReconcile_ExternalFlagRenamePreservesUID(t *testing.T) {
	box, idx, folder := recSetup(t)
	deliverToNew(t, box, "1700000001.MDA.host", "body\n")
	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	before, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	uid, oldName := before[0].UID, storedName(t, box, "INBOX", before[0])

	// A second MUA marks it \Seen by renaming X:2, -> X:2,S (base unchanged).
	curDir := filepath.Join(box.folderPath("INBOX"), "cur")
	newName := oldName + "S"
	if err := os.Rename(filepath.Join(curDir, oldName), filepath.Join(curDir, newName)); err != nil {
		t.Fatal(err)
	}

	st, err := box.ReconcileIndex(idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 0 || st.Expunged != 0 || st.Updated != 1 {
		t.Fatalf("stats = %+v, want only updated=1 (no churn)", st)
	}
	after, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(after) != 1 {
		t.Fatalf("message churned: %d records", len(after))
	}
	m := msgByUID(after, uid)
	if m == nil {
		t.Fatalf("UID %d lost after flag rename", uid)
	}
	if got := storedName(t, box, "INBOX", m); got != newName {
		t.Fatalf("the record resolves to %q, want %q", got, newName)
	}
	seen := false
	for _, f := range m.Flags {
		if f == `\Seen` {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("on-disk \\Seen not adopted: %+v", m.Flags)
	}
	// The message must still be fetchable at its new name.
	if rc, err := box.Fetch("INBOX", newName, false); err != nil {
		t.Fatalf("fetch after rename: %v", err)
	} else {
		rc.Close()
	}
}

// F4: a file removed out of band is expunged incrementally with a QRESYNC
// tombstone, so Vanished reports it.
func TestReconcile_VanishedFileWritesTombstone(t *testing.T) {
	box, idx, folder := recSetup(t)
	deliverToNew(t, box, "1700000002.MDA.host", "body\n")
	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	uid := msgs[0].UID
	if err := mailbox.RemoveMessage(box, "INBOX", msgs[0]); err != nil {
		t.Fatal(err)
	}

	st, err := box.ReconcileIndex(idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if st.Expunged != 1 || st.Imported != 0 {
		t.Fatalf("stats = %+v, want expunged=1", st)
	}
	vanished, err := idx.Vanished(folder.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range vanished {
		if v == uid {
			found = true
		}
	}
	if !found {
		t.Fatalf("no QRESYNC tombstone for expunged UID %d: %v", uid, vanished)
	}
}

// Concern #3: a tracked message whose on-disk name is unchanged keeps its index
// flags — the reconcile must not revert a client's \Seen back to the flagless
func TestReconcile_ImportCarriesGUIDAfterBackfill(t *testing.T) {
	box, idx, folder := recSetup(t)

	// Bring the folder to the state a backfilled mailbox is in.
	if err := idx.SetGUIDs(folder.ID, nil); err != nil {
		t.Fatalf("mark backfilled: %v", err)
	}
	need, err := idx.GUIDBackfillNeeded(folder.ID)
	if err != nil {
		t.Fatalf("needed: %v", err)
	}
	if need {
		t.Fatal("folder should be marked backfilled for this case")
	}

	deliverToNew(t, box, "1700000001.MDA.host", "body\n")
	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].GUID == ([16]byte{}) {
		t.Fatal("imported message has a zero GUID")
	}
	scanned, err := box.Scan("INBOX")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(scanned) != 1 || scanned[0].GUID != msgs[0].GUID {
		t.Errorf("index GUID %x does not match storage", msgs[0].GUID)
	}
}

// Two names for one base is what an interrupted flag change leaves behind.
// They must collapse into one record, or expunging either deletes the body the
// other still points at.
func TestReconcile_SameBaseTwiceImportsOnce(t *testing.T) {
	box, idx, folder := recSetup(t)

	const base = "1700000002.MDA.host"
	cur := filepath.Join(box.folderPath("INBOX"), "cur")
	for _, trailer := range []string{":2,", ":2,S"} {
		if err := os.WriteFile(filepath.Join(cur, base+trailer), []byte("body\n"), 0o600); err != nil {
			t.Fatalf("stage %s: %v", trailer, err)
		}
	}

	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 {
		names := make([]string, 0, len(msgs))
		for _, m := range msgs {
			names = append(names, storedName(t, box, "INBOX", m))
		}
		t.Fatalf("got %d records for one message: %v", len(msgs), names)
	}
}

func TestReconcile_RestampsZeroGUIDBehindCompleteMarker(t *testing.T) {
	box, idx, folder := recSetup(t)
	name, _, want, err := box.Save("INBOX", strings.NewReader("body\n"), 1, 5, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	recAppend(t, box, idx, folder, &mailbox.MessageMeta{
		UID: 1, Filename: name, Size: 5, VSize: 5,
	})
	if err := idx.SetGUIDs(folder.ID, nil); err != nil {
		t.Fatalf("mark complete: %v", err)
	}
	need, err := idx.GUIDBackfillNeeded(folder.ID)
	if err != nil {
		t.Fatalf("needed: %v", err)
	}
	if need {
		t.Fatal("folder should be marked complete for this case")
	}

	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].GUID != want {
		t.Errorf("GUID = %x, want the one storage reports (%x)", msgs[0].GUID, want)
	}
}

// recAppend records a message the way every caller does: the list learns the
// uid, and the record keeps no name.
func recAppend(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, folder *mailbox.Folder, m *mailbox.MessageMeta) {
	t.Helper()
	guid := m.GUID
	if err := mailbox.NameSaved(box, folder.Name, m); err != nil {
		t.Fatalf("name uid %d: %v", m.UID, err)
	}
	m.GUID = guid
	if err := idx.AppendMessage(folder.ID, m); err != nil {
		t.Fatalf("append uid %d: %v", m.UID, err)
	}
}
