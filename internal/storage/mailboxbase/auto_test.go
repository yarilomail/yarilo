package mailboxbase_test

import (
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Every open makes a configured mailbox, whichever server opens it; a
// diagnostic, which reads an account as it is, makes nothing.
func TestAConfiguredMailboxIsMadeByEveryOpenButADiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []mailboxbase.BoxOption
		call func(b *mailboxbase.Box) error
		made bool
	}{
		{"list", nil, func(b *mailboxbase.Box) error { _, err := b.ListFolders(); return err }, true},
		{"exists", nil, func(b *mailboxbase.Box) error { _, err := b.FolderExists("Sent"); return err }, true},
		{"open", nil, func(b *mailboxbase.Box) error { _, err := b.Folder("Sent", 0); return err }, true},
		{"delivery", []mailboxbase.BoxOption{mailboxbase.SaveOnly()}, func(b *mailboxbase.Box) error { _, err := b.Folder("Sent", 0); return err }, true},
		{"diagnostic list", []mailboxbase.BoxOption{mailboxbase.ReadOnly()}, func(b *mailboxbase.Box) error { _, err := b.ListFolders(); return err }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &mailbox.UserInfo{Username: "u@x", Home: filepath.Join(t.TempDir(), "u"), Driver: "maildir"}
			store := maildir.New().OpenUser(info)
			if err := store.Init(); err != nil {
				t.Fatal(err)
			}
			idx := file.New().OpenUser(info)
			t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
			opts := append(tc.opts, mailboxbase.WithAuto(mailboxbase.Auto{
				Mailboxes: map[string]mailbox.AutoMailbox{"Sent": {Auto: mailbox.AutoCreate}},
			}))
			if err := tc.call(mailboxbase.Open(store, idx, opts...)); err != nil {
				t.Fatal(err)
			}
			if made, _ := store.FolderExists("Sent"); made != tc.made {
				t.Fatalf("Sent exists: %v, want %v", made, tc.made)
			}
		})
	}
}
