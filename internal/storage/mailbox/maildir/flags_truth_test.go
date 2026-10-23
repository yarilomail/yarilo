package maildir

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The name carries the flags, and the name is the truth: a file back at ":2,"
// takes \Seen off the record, where we used to keep the index's answer (#1700).
func TestTheNameTakesTheSeenFlagOffTheRecord(t *testing.T) {
	box, idx, folder := recSetup(t)
	name, _, _, err := box.Save("INBOX", strings.NewReader("body\n"), 0, 5, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := box.AssignUID("INBOX", name, 1); aerr != nil {
		t.Fatalf("assign uid: %v", aerr)
	}
	recAppend(t, box, idx, folder, name, &mailbox.MessageMeta{
		UID: 1, Size: 5, VSize: 5, Flags: []string{`\Seen`},
	})

	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 1 {
		t.Fatalf("record churned: %+v", msgs)
	}
	for _, f := range msgs[0].Flags {
		if f == `\Seen` {
			t.Errorf("the record still says %s, and the file's name says nothing", f)
		}
	}
}

// Unless the flags never reached the disk: a rename that failed marks the
// record dirty, and a sync leaves it alone until the write lands (#1700).
func TestADirtyRecordKeepsItsFlagsUntilTheRenameLands(t *testing.T) {
	box, idx, folder := recSetup(t)
	name, _, _, err := box.Save("INBOX", strings.NewReader("body\n"), 0, 5, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := box.AssignUID("INBOX", name, 1); aerr != nil {
		t.Fatalf("assign uid: %v", aerr)
	}
	recAppend(t, box, idx, folder, name, &mailbox.MessageMeta{
		UID: 1, Size: 5, VSize: 5, Flags: []string{`\Seen`},
	})
	marker, ok := idx.(mailbox.FlagsDirtyMarker)
	if !ok {
		t.Fatal("the index cannot say a record's flags are not on disk")
	}
	if err := marker.SetFlagsDirty(folder.ID, 1, true); err != nil {
		t.Fatal(err)
	}

	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if !hasFlagIn(msgs[0].Flags, `\Seen`) {
		t.Errorf("the flag the client was told about was taken off by a sync: %v", msgs[0].Flags)
	}

	// The write lands: the name carries it, the bit is cleared, and the two agree.
	stored := storedName(t, box, "INBOX", msgs[0])
	if _, err := box.WriteFlags("INBOX", stored, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	if err := marker.SetFlagsDirty(folder.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, _ = idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if !hasFlagIn(msgs[0].Flags, `\Seen`) {
		t.Errorf("after the rename landed the record lost the flag: %v", msgs[0].Flags)
	}
}

// A record the list does not name opens nothing: left in place, since its file
// may be there unrecorded, and said out loud once (#1693, #1700).
func TestARecordTheListDoesNotNameIsLeftAloneAndReported(t *testing.T) {
	ForgetReports()
	box, idx, folder := recSetup(t)
	if _, _, _, err := box.Save("INBOX", strings.NewReader("body\n"), 0, 5, nil, nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	// Recorded in the index and nowhere else: no list entry for this uid.
	if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{
		UID: 1, Size: 5, VSize: 5,
	}); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	for i := 0; i < 2; i++ {
		if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	found := false
	for _, m := range msgs {
		if m.UID == 1 {
			found = true
		}
	}
	if !found {
		t.Error("the record was expunged; its file may be on disk under a name nobody recorded")
	}
	if got := strings.Count(logged.String(), "names no file for this record"); got != 1 {
		t.Errorf("the record was reported %d times over two passes, want once", got)
	}
}

// A record the list does not name stays nameless whatever sits beside the
// index, and the counter moves (#1801).
func TestANamelessRecordIsNotNamedFromASidecar(t *testing.T) {
	box, idx, folder := recSetup(t)
	name, _, _, err := box.Save("INBOX", strings.NewReader("body\n"), 0, 5, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := box.AssignUID("INBOX", name, 1); aerr != nil {
		t.Fatalf("assign uid: %v", aerr)
	}
	// The record, with no row naming it.
	if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: 7, Size: 5, VSize: 5}); err != nil {
		t.Fatal(err)
	}
	dir := idx.(interface{ IndexDirFor(string) string }).IndexDirFor("INBOX")
	if err := os.WriteFile(filepath.Join(dir, "yarilo.index.names"),
		[]byte("7\t"+name+"\t5\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ForgetReports()
	was := testutil.ToFloat64(metricRecordWithoutRow)
	if _, err := mailboxbase.Open(box, idx).Folder("INBOX", folder.UIDValidity); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metricRecordWithoutRow); got <= was {
		t.Errorf("the nameless record was not reported: counter %v, was %v", got, was)
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	for _, m := range msgs {
		if m.UID != 7 {
			continue
		}
		if got, _ := box.RecordPath("INBOX", m); got != "" {
			t.Errorf("uid 7 resolved to %q; the name came from the sidecar", got)
		}
	}
}

// countingIndex counts the index writes a sync pass makes.
type countingIndex struct {
	mailbox.UserIndex
	single, commits int
}

func (c *countingIndex) ExpungeMessage(folderID uint64, uid uint32) error {
	c.single++
	return c.UserIndex.ExpungeMessage(folderID, uid)
}

func (c *countingIndex) UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	c.single++
	return c.UserIndex.UpdateFlags(folderID, uid, flags, keywords)
}

func (c *countingIndex) Begin(folderID uint64) (mailbox.IndexTx, error) {
	tx, err := c.UserIndex.Begin(folderID)
	if err != nil {
		return nil, err
	}
	return &countingTx{IndexTx: tx, c: c}, nil
}

type countingTx struct {
	mailbox.IndexTx
	c *countingIndex
}

func (t *countingTx) Commit() (mailbox.TxResult, error) {
	t.c.commits++
	return t.IndexTx.Commit()
}

// A pass that takes flags from several names and drops a vanished message writes
// it all in one transaction, not one index write per message (#714).
func TestASyncPassWritesItsFlagsInOneTransaction(t *testing.T) {
	box, idx, folder := recSetup(t)
	for uid := uint32(1); uid <= 5; uid++ {
		name, _, _, err := box.Save("INBOX", strings.NewReader("body\n"), 0, 5, nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if _, aerr := box.AssignUID("INBOX", name, uid); aerr != nil {
			t.Fatalf("assign uid: %v", aerr)
		}
		recAppend(t, box, idx, folder, name, &mailbox.MessageMeta{
			UID: uid, Size: 5, VSize: 5, Flags: []string{`\Seen`},
		})
	}
	gone, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 5, To: 5}})
	var victim string
	base := filepath.Base(storedName(t, box, "INBOX", gone[0]))
	_ = filepath.Walk(box.mailPath, func(p string, fi os.FileInfo, _ error) error {
		if fi != nil && !fi.IsDir() && fi.Name() == base {
			victim = p
		}
		return nil
	})
	if victim == "" {
		t.Fatalf("no file %s under %s", base, box.mailPath)
	}
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	counted := &countingIndex{UserIndex: idx}
	if _, err := box.ReconcileIndex(mailboxbase.Open(box, counted), counted, folder); err != nil {
		t.Fatal(err)
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 4 {
		t.Fatalf("%d records after the pass, want 4: the vanished one stayed", len(msgs))
	}
	for _, m := range msgs {
		if hasFlagIn(m.Flags, `\Seen`) {
			t.Fatalf("uid %d kept \\Seen the name does not carry", m.UID)
		}
	}
	if counted.single != 0 || counted.commits != 1 {
		t.Errorf("4 flag changes and an expunge took %d single writes and %d transactions, want 0 and 1", counted.single, counted.commits)
	}
}
