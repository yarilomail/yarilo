package lmtp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A delivery to a folder that is missing: a configured auto mailbox is made by
// the open itself; any other one by the two lda_mailbox knobs, else INBOX.
func TestADeliveryToAMissingFolder(t *testing.T) {
	for _, drv := range []struct {
		name string
		new  func() mailbox.MailboxBackend
	}{
		{"maildir", func() mailbox.MailboxBackend { return maildir.New() }},
		{"sdbox", func() mailbox.MailboxBackend { return dboxv2.New() }},
		{"mdbox", func() mailbox.MailboxBackend { return mdbox.New() }},
	} {
		for _, tc := range []struct {
			name        string
			auto        map[string]config.NamespaceMailboxConfig
			create, sub bool
			wantMade    bool
			wantSubs    string
		}{
			{name: "no knob, no auto: INBOX"},
			{name: "autocreate: made, not subscribed", create: true, wantMade: true},
			{name: "autocreate and autosubscribe: made and subscribed", create: true, sub: true,
				wantMade: true, wantSubs: "Lists\n"},
			{name: "auto subscribe: made and subscribed with the knobs off",
				auto: map[string]config.NamespaceMailboxConfig{"Lists": {Auto: "subscribe"}}, wantMade: true, wantSubs: "Lists\n"},
			{name: "auto elsewhere does not make another folder",
				auto: map[string]config.NamespaceMailboxConfig{"Sent": {Auto: "subscribe"}}},
		} {
			t.Run(drv.name+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
				backend := drv.new()
				s := &session{opts: Options{
					Mailbox: backend, Index: fileindex.New(), Resolver: resolver,
					Config: config.LMTPProtocolConfig{SaveToDetailMailbox: true,
						LDAMailboxAutocreate: tc.create, LDAMailboxAutosubscribe: tc.sub},
					Namespaces: []config.NamespaceConfig{
						{Type: "personal", Prefix: "", Separator: "/", Inbox: true, Mailboxes: tc.auto},
					},
				}}
				s.from = "sender@x"
				s.rcpts = []string{"alice+Lists@example.com"}
				if err := s.LMTPData(strings.NewReader("Subject: missing\r\n\r\nbody\r\n"), &statusSink{}); err != nil {
					t.Fatalf("LMTPData: %v", err)
				}
				ui := resolver.UserInfo("alice@example.com", "")
				store := backend.OpenUser(ui)
				defer store.Close() //nolint:errcheck
				made, _ := store.FolderExists("Lists")
				if made != tc.wantMade {
					t.Fatalf("Lists exists: %v, want %v", made, tc.wantMade)
				}
				want := map[bool]string{true: "Lists", false: "INBOX"}[tc.wantMade]
				if n := messagesIn(t, ui, want); n != 1 {
					t.Fatalf("%s holds %d messages, want the delivery there", want, n)
				}
				subs, _ := os.ReadFile(filepath.Join(mailbox.ControlRoot(ui), "subscriptions"))
				if string(subs) != tc.wantSubs {
					t.Fatalf("subscriptions holds %q, want %q", subs, tc.wantSubs)
				}
			})
		}
	}
}

// messagesIn counts a folder's records by the index the delivery wrote.
func messagesIn(t *testing.T, ui *mailbox.UserInfo, folder string) int {
	t.Helper()
	idx := fileindex.New().OpenUser(ui)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder(folder, 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	return len(msgs)
}
