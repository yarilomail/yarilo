package lmtp

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// refusingIndex refuses every record; embedding hides the naming append, so
// each driver reaches the plain one.
type refusingIndex struct{ mailbox.UserIndex }

var errRefused = errors.New("record refused")

func (refusingIndex) AllocateAndAppend(uint64, *mailbox.MessageMeta) error { return errRefused }

// namedThenRefused lets the driver name the body under its uid, then refuses
// the record: the body has left where Save put it.
type namedThenRefused struct {
	refusingIndex
	named *bool
}

func (n namedThenRefused) AllocateAndAppendNamed(_ uint64, m *mailbox.MessageMeta, name func(uint32) (string, error)) error {
	*n.named = true
	m.UID = 1
	if _, err := name(m.UID); err != nil {
		return err
	}
	return errRefused
}

// A delivery whose record is refused leaves no body behind, on every driver.
func TestARefusedRecordLeavesNoBody(t *testing.T) {
	for _, tc := range []struct {
		driver string
		new    func() mailbox.MailboxBackend
		// names is whether the driver renames the body inside the cycle; mdbox
		// does not, and a change there has to come past this row.
		names bool
	}{
		{"maildir", func() mailbox.MailboxBackend { return maildir.New() }, true},
		{"sdbox", func() mailbox.MailboxBackend { return dboxv2.New() }, true},
		{"mdbox", func() mailbox.MailboxBackend { return mdbox.New() }, false},
	} {
		for _, afterName := range []bool{false, true} {
			stage := map[bool]string{false: "before-name", true: "after-name"}[afterName]
			t.Run(tc.driver+"/"+stage, func(t *testing.T) {
				info := &mailbox.UserInfo{
					Username: "alice@x", Home: filepath.Join(t.TempDir(), "alice"), Driver: tc.driver,
				}
				store := tc.new().OpenUser(info)
				defer store.Close() //nolint:errcheck
				if err := store.Init(); err != nil {
					t.Fatal(err)
				}
				ui := fileindex.New().OpenUser(info)
				defer ui.Close() //nolint:errcheck

				raw := "From: a@b\r\nSubject: refused\r\n\r\nrefused-body-marker\r\n"
				var named bool
				var idx mailbox.UserIndex = refusingIndex{ui}
				if afterName {
					idx = namedThenRefused{refusingIndex{ui}, &named}
				}
				_, _, _, err := deliverOne(mailboxbase.Open(store, idx), "INBOX",
					bytes.NewReader([]byte(raw)), int64(len(raw)), nil, info.Username, "x@y", nil)
				if !errors.Is(err, errRefused) {
					t.Fatalf("deliver = %v, want the refusal", err)
				}
				if afterName && named != tc.names {
					t.Fatalf("the driver named the body: %v, want %v", named, tc.names)
				}
				// mdbox drops a reference and leaves the bytes to its purge.
				if p, ok := mailbox.Driver(store).(interface {
					Purge() (mdbox.PurgeStats, error)
				}); ok {
					if _, err := p.Purge(); err != nil {
						t.Fatal(err)
					}
				}
				if held := filesHolding(t, info.Home, "refused-body-marker"); len(held) != 0 {
					t.Fatalf("a refused record left its body on disk: %v", held)
				}
			})
		}
	}
}

// filesHolding names every file under root whose bytes contain marker: a scan
// reads neither tmp/ nor a dbox temp, where an unrecorded body sits.
func filesHolding(t *testing.T, root, marker string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(b, []byte(marker)) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
