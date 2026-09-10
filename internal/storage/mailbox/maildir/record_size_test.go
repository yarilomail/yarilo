package maildir_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The size comes from where the driver keeps it: the name carries both numbers,
// and a name without them is measured (#1726).
func TestTheSizeComesFromTheName(t *testing.T) {
	// A lone LF makes the two numbers differ, which is the whole point of W=.
	const body = "From: a@b\n\nx\n"
	for _, tc := range []struct {
		name        string
		inName      bool
		claim       uint32
		recordSize  uint32
		recordVSize uint32
		wantSize    uint32
		wantVSize   uint32
	}{
		{name: "the record holds both", inName: true, recordSize: 5, recordVSize: 7, wantSize: 5, wantVSize: 7},
		{name: "the name holds both", inName: true, wantSize: 13, wantVSize: 16},
		{name: "neither, so the file is measured", inName: false, wantSize: 13, wantVSize: 16},
		// The name is what the reference trusts, so a name that disagrees with
		// the file is what tells the parse apart from measuring it.
		{name: "the name disagrees with the file", inName: true, claim: 99, wantSize: 99, wantVSize: 102},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			idx := file.New().OpenUser(info)
			defer idx.Close() //nolint:errcheck
			f, err := idx.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}

			base := "1700000001.M1P1_1.host"
			switch {
			case tc.claim != 0:
				base += fmt.Sprintf(",S=%d,W=%d", tc.claim, tc.claim+3)
			case tc.inName:
				base += fmt.Sprintf(",S=%d,W=%d", len(body), len(body)+3)
			}
			name := base + ":2,"
			if err := os.WriteFile(filepath.Join(home, "Maildir", "cur", name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			m := &mailbox.MessageMeta{UID: 1, Size: tc.recordSize, VSize: tc.recordVSize}
			if err := mbox.RecordSaved(idx, box, f.ID, "INBOX", name, m); err != nil {
				t.Fatal(err)
			}

			size, vsize, serr := mbox.MessageSize(box, "INBOX", m)
			if serr != nil {
				t.Fatalf("size: %v", serr)
			}
			if size != tc.wantSize || vsize != tc.wantVSize {
				t.Errorf("size = %d/%d, want %d/%d", size, vsize, tc.wantSize, tc.wantVSize)
			}
		})
	}
}

// A dbox record answers from itself: the sizes are in the index there, and a
// read of the file to repeat them is one the reference never makes.
func TestADboxSizeIsNotReadFromTheFile(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"}
	box := dboxv2.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := file.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: a@b\r\n\r\nx\r\n"
	saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := mbox.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	// Proven by taking the storage away: a record holding both numbers is
	// answered from itself, and nothing opens the message to repeat them.
	if rerr := box.Remove("INBOX", saved); rerr != nil {
		t.Fatal(rerr)
	}
	size, got, serr := mbox.MessageSize(box, "INBOX", m)
	if serr != nil {
		t.Fatal(serr)
	}
	if size != uint32(len(body)) || got != vsize {
		t.Errorf("size = %d/%d, want %d/%d", size, got, len(body), vsize)
	}
}
