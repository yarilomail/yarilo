package pop3

import (
	"path/filepath"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// %f and %m are the two UIDL variables that read a message's name on disk. The
// record stopped carrying one, so they ask the driver: a UIDL that changed
// shape would have every client download the mailbox again (#1700).
func TestTheUIDLNameVariablesReadTheNameOnDisk(t *testing.T) {
	for _, tc := range []struct {
		driver string
		new    func() mailbox.MailboxBackend
	}{
		{"maildir", func() mailbox.MailboxBackend { return maildir.New() }},
		{"sdbox", func() mailbox.MailboxBackend { return dboxv2.New() }},
		{"mdbox", func() mailbox.MailboxBackend { return mdbox.New() }},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			root := t.TempDir()
			info := &mailbox.UserInfo{
				Username: "u1@example.com", Home: filepath.Join(root, "u1"), Driver: tc.driver,
			}
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
			saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), 0, int64(len(raw)), nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			m := &mailbox.MessageMeta{Filename: saved, Size: uint32(len(raw)), VSize: vsize, GUID: guid}
			if err := mailbox.RecordSaved(idx, box, f.ID, "INBOX", m); err != nil {
				t.Fatal(err)
			}

			want, err := mailbox.MessagePath(box, "INBOX", m)
			if err != nil || want == "" {
				t.Fatalf("the driver cannot name the message: %q %v", want, err)
			}
			s := &session{box: box, srv: &Server{opts: Options{UIDLFormat: "%f"}}}
			if got := s.formatUIDL(m); got != want {
				t.Errorf("%%f = %q, want %q", got, want)
			}
			s.srv.opts.UIDLFormat = "%m"
			got := s.formatUIDL(m)
			if len(got) != 32 {
				t.Errorf("%%m = %q, want the digest of %q", got, want)
			}
		})
	}
}
