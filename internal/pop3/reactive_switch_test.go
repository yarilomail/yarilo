package pop3

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// With dbox_reactive_rebuild off, a login must leave the records of missing
// files alone: the operator turned it off while the files are being restored.
func TestPOP3LoginHealHonoursDboxReactiveRebuild(t *testing.T) {
	for _, tc := range []struct {
		driver  string
		new     func() mailbox.MailboxBackend
		file    string
		enabled bool
		want    int
	}{
		{"sdbox", func() mailbox.MailboxBackend { return dboxv2.New() }, "u.1", false, 1},
		{"sdbox", func() mailbox.MailboxBackend { return dboxv2.New() }, "u.1", true, 0},
		{"mdbox", func() mailbox.MailboxBackend { return mdbox.New() }, "m.1", false, 1},
		{"mdbox", func() mailbox.MailboxBackend { return mdbox.New() }, "m.1", true, 0},
	} {
		name := tc.driver + "/off"
		if tc.enabled {
			name = tc.driver + "/on"
		}
		t.Run(name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "u1")
			info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: tc.driver}
			box := tc.new().OpenUser(info)
			defer box.Close() //nolint:errcheck
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			idx := fileindex.New().OpenUser(info)
			defer idx.Close() //nolint:errcheck
			f, err := idx.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			raw := "From: a@b\r\n\r\nbody\r\n"
			saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), 0, int64(len(raw)), nil, nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			m := &mailbox.MessageMeta{Size: uint32(len(raw)), VSize: vsize, GUID: guid}
			if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
				t.Fatal(err)
			}
			removeOne(t, home, tc.file)
			if err := idx.(mailbox.CorruptionMarker).MarkFolderCorrupt(f.ID); err != nil {
				t.Fatal(err)
			}

			s := &session{
				box:      mailboxbase.Open(box, idx),
				userInfo: info,
				srv:      &Server{opts: Options{DboxReactiveRebuild: tc.enabled}},
			}
			if err := s.loadMailbox(); err != nil {
				t.Fatal(err)
			}
			if len(s.msgs) != tc.want {
				t.Errorf("login saw %d messages, want %d", len(s.msgs), tc.want)
			}
			after, err := idx.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			if after.Fsckd == tc.enabled {
				t.Errorf("FSCKD = %v after login with the switch %v", after.Fsckd, tc.enabled)
			}
		})
	}
}

// removeOne deletes the single file named base under root.
func removeOne(t *testing.T, root, base string) {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == base {
			found = append(found, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d files named %s under %s, want 1", len(found), base, root)
	}
	if err := os.Remove(found[0]); err != nil {
		t.Fatal(err)
	}
}
