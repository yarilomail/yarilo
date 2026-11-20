package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// stripVsizeExtension rewrites a folder's base index without the vsize record
// extension, which is what a folder written before it existed holds.
func stripVsizeExtension(t *testing.T, path string) {
	t.Helper()
	f, err := mailindex.Open(path)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	kept := f.Extensions[:0]
	found := false
	for _, e := range f.Extensions {
		if e.Name == "vsize" {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		t.Fatal("the folder has no vsize extension to strip; the fixture proves nothing")
	}
	f.Extensions = kept
	for _, rec := range f.Records {
		delete(rec.Ext, "vsize")
	}
	in := f.ToRecreateInput(path)
	in.Extensions = kept
	layout, lerr := mailindex.ComputeRecordLayout(kept)
	if lerr != nil {
		t.Fatal(lerr)
	}
	in.Header.RecordSize = layout.RecordSize
	extBytes, eerr := mailindex.EncodeExtHeaders(layout.Extensions)
	if eerr != nil {
		t.Fatal(eerr)
	}
	in.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	if _, err := mailindex.Recreate(in); err != nil {
		t.Fatalf("rewrite index: %v", err)
	}
	for _, suffix := range []string{".log", ".cache"} {
		_ = os.Remove(path + suffix)
	}
}

// recordSizeOnDisk is the layout the file declares.
func recordSizeOnDisk(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := mailindex.Open(path)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	layout, err := mailindex.ComputeRecordLayout(f.Extensions)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	return layout.RecordSize
}

// A folder written before the vsize extension existed gets it, and one pass
// fills what it holds; the record size on disk moves with it (#1752).
func TestAFolderWithoutTheVsizeExtensionGetsItAndIsFilledOnce(t *testing.T) {
	box, idx, f, path := vsizeFixture(t, "u1@example.com", 3)
	before := recordSizeOnDisk(t, path)

	filled, err := mailboxbase.FillSizelessRecords(idx, box, f)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 3 {
		t.Fatalf("the pass filled %d records, want 3", filled)
	}

	mdbox.ResetStorageSizeReads()
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if got := mailboxbase.RFC822SizeOf(box, "INBOX", m); got == 0 {
			t.Errorf("uid %d still answers size 0", m.UID)
		}
	}
	if got := mdbox.StorageSizeReads(); got != 0 {
		t.Errorf("%d size questions reached the message body after the pass", got)
	}
	// Kept, not only held: a cold open, a new index that remembers nothing,
	// reads every size from the base and its journal and opens no body. Then
	// the fold moves the layout on disk, and the sizes stay what they were
	// (reviewer, 2026-11-20).
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	sizes := func(what string) map[uint32]uint32 {
		t.Helper()
		info := &mailbox.UserInfo{Username: "u1@example.com", Home: homeOf(box), Driver: "mdbox"}
		cold := file.New().OpenUser(info)
		defer cold.Close() //nolint:errcheck
		cf, err := cold.OpenFolder("INBOX", 1)
		if err != nil {
			t.Fatal(err)
		}
		mdbox.ResetStorageSizeReads()
		got, err := cold.GetMessages(cf.ID, mailbox.SeqSet{})
		if err != nil {
			t.Fatal(err)
		}
		out := map[uint32]uint32{}
		for _, m := range got {
			if out[m.UID] = mailboxbase.RFC822SizeOf(box, "INBOX", m); out[m.UID] == 0 {
				t.Errorf("%s: uid %d answers size 0", what, m.UID)
			}
		}
		if n := mdbox.StorageSizeReads(); n != 0 {
			t.Errorf("%s: %d size questions reached the message body", what, n)
		}
		if what == "a cold open" {
			if err := cold.(interface{ OptimizeIndex(uint64) error }).OptimizeIndex(cf.ID); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	kept := sizes("a cold open")
	if after := recordSizeOnDisk(t, path); after <= before {
		t.Errorf("after the fold the record size on disk is %d, was %d: the extension did not move the layout", after, before)
	}
	if folded := sizes("after the fold"); fmt.Sprint(folded) != fmt.Sprint(kept) || len(kept) != 3 {
		t.Errorf("sizes %v after the fold, %v before", folded, kept)
	}
}

// The pass does not repeat: a second open finds nothing sizeless and opens no
// body at all (#1752).
func TestTheFillDoesNotRepeatOnTheNextOpen(t *testing.T) {
	box, idx, f, _ := vsizeFixture(t, "u2@example.com", 2)
	if _, err := mailboxbase.FillSizelessRecords(idx, box, f); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	info := &mailbox.UserInfo{Username: "u2@example.com", Home: homeOf(box), Driver: "mdbox"}
	idx2 := file.New().OpenUser(info)
	defer idx2.Close() //nolint:errcheck
	f2, err := idx2.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	mdbox.ResetStorageSizeReads()
	filled, err := mailboxbase.FillSizelessRecords(idx2, box, f2)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 0 {
		t.Errorf("the second open filled %d records; the first pass is the only one", filled)
	}
	if got := mdbox.StorageSizeReads(); got != 0 {
		t.Errorf("the second open opened %d bodies", got)
	}
}

// findIndex is where this driver put the folder's index.
func findIndex(t *testing.T, home string) string {
	t.Helper()
	var found string
	err := filepath.Walk(home, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() && filepath.Base(p) == "yarilo.index" && found == "" {
			found = p
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("no index under %s: %v", home, err)
	}
	return found
}

// vsizeFixture builds a folder whose index predates the vsize extension.
func vsizeFixture(t *testing.T, user string, n int) (mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder, string) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: user, Home: home, Driver: "mdbox"}
	box := mdbox.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := file.New().OpenUser(info)
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("From: a@b\r\nSubject: m%d\r\n\r\nbody %d\r\n", i, i)
		saved, _, guid, serr := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved,
			&mailbox.MessageMeta{Size: uint32(len(body)), VSize: uint32(len(body)), GUID: guid}); err != nil {
			t.Fatal(err)
		}
	}
	// Fold the log into the base: the strip rewrites the base, and records
	// still sitting in the log would be dropped with it.
	if err := idx.RecomputeVSize(f.ID); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	path := findIndex(t, home)
	stripVsizeExtension(t, path)
	idx2 := file.New().OpenUser(info)
	t.Cleanup(func() { _ = idx2.Close() })
	f2, err := idx2.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	fixtureHomes[box] = home
	return box, idx2, f2, path
}

// fixtureHomes remembers where a fixture put a handle's mail, so a second open
// can find it.
var fixtureHomes = map[mailbox.UserMailbox]string{}

func homeOf(box mailbox.UserMailbox) string { return fixtureHomes[box] }
