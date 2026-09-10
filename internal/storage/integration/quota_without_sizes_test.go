package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/internal/storage/mbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// sizelessMaildir is the shape an older build and every adopted store leave: the
// files carry their sizes, the records do not.
func sizelessMaildirAt(t *testing.T, n int) (mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder, uint64, string) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: a@b\r\n\r\nbody\r\n"
	var want uint64
	namer := mailbox.Driver(box).(mailbox.UIDNamer)
	for i := 1; i <= n; i++ {
		base := fmt.Sprintf("17000000%02d.M1P1_%d.host,S=%d,W=%d", i, i, len(body), len(body))
		name := base + ":2,"
		if werr := os.WriteFile(filepath.Join(home, "Maildir", "cur", name), []byte(body), 0o600); werr != nil {
			t.Fatal(werr)
		}
		// The record carries no size at all, which is what the sum reads as zero.
		if aerr := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i)}); aerr != nil {
			t.Fatal(aerr)
		}
		if _, aerr := namer.AssignUID("INBOX", name, uint32(i)); aerr != nil {
			t.Fatal(aerr)
		}
		want += uint64(len(body))
	}
	return box, idx, f, want, home
}

// stripRecordSizes clears the per-record size and leaves the folder aggregate,
// which is the state a build that maintained only the aggregate wrote.
func stripRecordSizes(t *testing.T, path string) {
	t.Helper()
	fi, err := mailindex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range fi.Records {
		delete(rec.Ext, "vsize")
	}
	if _, err := mailindex.Recreate(fi.ToRecreateInput(path)); err != nil {
		t.Fatal(err)
	}
}

func folderSum(t *testing.T, idx mailbox.UserIndex, f *mailbox.Folder) uint64 {
	t.Helper()
	vs, ok := idx.(interface {
		FolderVSize(uint64) (uint64, uint32, error)
	})
	if !ok {
		t.Fatal("the index does not answer for the folder's size")
	}
	bytes, _, err := vs.FolderVSize(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return bytes
}

// The driver fills what the records do not say, and the sum is the mail (#1728).
func TestTheDriverFillsTheSizesTheRecordsLack(t *testing.T) {
	box, idx, f, want, _ := sizelessMaildirAt(t, 4)
	if got := folderSum(t, idx, f); got != 0 {
		t.Fatalf("the fixture is not sizeless: the folder already sums %d", got)
	}
	if _, err := mbox.FillSizelessRecords(idx, box, f); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if got := folderSum(t, idx, f); got != want {
		t.Errorf("the folder sums %d bytes, the mail is %d", got, want)
	}
}

// A flush does not re-derive the sum, over the state an older build left: the
// aggregate right, the records saying nothing, no fill on this path (#1728).
func TestAFlushKeepsTheSum(t *testing.T) {
	_, idx, f, want, home := sizelessMaildirAt(t, 4)
	// The aggregate as that build maintained it, over records that carry none:
	// written into the records, then taken back out of them on disk.
	stamper := idx.(mailbox.SizeStamper)
	if _, err := stamper.StampSizes(f.ID, map[uint32]uint32{1: 19, 2: 19, 3: 19, 4: 19}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	stripRecordSizes(t, filepath.Join(home, "yarilo.index"))
	idx = indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	before := folderSum(t, idx, f)
	if before != want {
		t.Fatalf("the folder sums %d before the flush, want %d", before, want)
	}
	// A write that rewrites the base, which is where the sum was re-derived.
	if err := idx.(mailbox.UIDNameMarker).MarkUIDNamed(f.ID); err != nil {
		t.Fatal(err)
	}
	if got := folderSum(t, idx, f); got != before {
		t.Errorf("the sum moved from %d to %d over a base rewrite", before, got)
	}
}

// A converted dbox store says no size in its records either, and the folder
// sums to nothing until the driver is asked (#1728).
func TestAdoptedDboxRecordsAreFilled(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"}
	box := dboxv2.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: a@b\r\n\r\nbody\r\n"
	var want uint64
	for i := 0; i < 3; i++ {
		saved, _, guid, serr := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		// What the conversion writes: the record names the message and says
		// nothing about its size.
		m := &mailbox.MessageMeta{GUID: guid}
		if rerr := mbox.RecordSaved(idx, box, f.ID, "INBOX", saved, m); rerr != nil {
			t.Fatal(rerr)
		}
		want += uint64(len(body))
	}
	if got := folderSum(t, idx, f); got != 0 {
		t.Fatalf("the fixture is not sizeless: the folder sums %d", got)
	}
	if _, err := mbox.FillSizelessRecords(idx, box, f); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if got := folderSum(t, idx, f); got != want {
		t.Errorf("the folder sums %d bytes, the mail is %d", got, want)
	}
}

// A name from another MTA carries no ,W=: the virtual size is counted from the
// file, never taken as the physical one (#1728).
func TestANameWithoutTheVirtualSizeIsCounted(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	// Bare LF: the wire size is larger than the file, which is the whole
	// difference between counting and stat.
	body := "From: a@b\n\nbody\n"
	name := "1700000001.M1P1_1.host:2,"
	if werr := os.WriteFile(filepath.Join(home, "Maildir", "cur", name), []byte(body), 0o600); werr != nil {
		t.Fatal(werr)
	}
	if aerr := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1}); aerr != nil {
		t.Fatal(aerr)
	}
	if _, aerr := mailbox.Driver(box).(mailbox.UIDNamer).AssignUID("INBOX", name, 1); aerr != nil {
		t.Fatal(aerr)
	}
	if _, ferr := mbox.FillSizelessRecords(idx, box, f); ferr != nil {
		t.Fatal(ferr)
	}
	wantV := uint64(len(body) + 3)
	if got := folderSum(t, idx, f); got != wantV {
		t.Errorf("the folder sums %d, want the wire size %d (the file is %d)", got, wantV, len(body))
	}
}
