package lmtp

import (
	"path/filepath"
	"testing"

	fileindex "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

type countingBackend struct {
	mailbox.MailboxBackend
	opened []*mailbox.UserInfo
}

func (b *countingBackend) OpenUser(ui *mailbox.UserInfo) mailbox.UserMailbox {
	b.opened = append(b.opened, ui)
	return b.MailboxBackend.OpenUser(ui)
}

// Delivery opens a namespace as a session does: its location expanded against
// the recipient, on the backend its prefix is given (#2605).
func TestDeliveryOpensANamespaceAsASessionDoes(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name     string
		override bool
		wantHome string
	}{
		{"location expanded against the recipient", false, filepath.Join(root, "alice", "team")},
		{"the prefix's backend", true, filepath.Join(root, "alice", "team")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global := &countingBackend{MailboxBackend: maildir.New()}
			prefixed := &countingBackend{MailboxBackend: maildir.New()}
			opts := Options{
				Mailbox: global,
				Index:   fileindex.New(),
				Namespaces: []config.NamespaceConfig{
					{Type: "personal", Prefix: "", Separator: "/"},
					{Type: "shared", Prefix: "Team/", Separator: "/", Location: "maildir:%h/team"},
				},
			}
			if tc.override {
				opts.NamespaceMailboxes = map[string]mailbox.MailboxBackend{"Team/": prefixed}
			}
			s := &session{opts: testOpts(opts)}
			rcptUI := &mailbox.UserInfo{Username: "alice@x", Home: filepath.Join(root, "alice"), MailPath: filepath.Join(root, "alice", "Maildir")}
			rcptBox := maildir.New().OpenUser(rcptUI)
			if err := rcptBox.Init(); err != nil {
				t.Fatal(err)
			}
			rcptMbox := mailboxbase.Open(rcptBox, fileindex.New().OpenUser(rcptUI))

			_, _, rel, done := s.deliveryTarget(rcptUI, rcptBox, rcptMbox, "Team/Plans", false)
			defer done()
			want, other := global, prefixed
			if tc.override {
				want, other = prefixed, global
			}
			if rel != "Plans" || len(want.opened) != 1 || len(other.opened) != 0 {
				t.Fatalf("rel %q, opened on the expected backend %d times, on the other %d", rel, len(want.opened), len(other.opened))
			}
			if got := want.opened[0].Home; got != tc.wantHome {
				t.Errorf("delivery opened %s, a session opens %s", got, tc.wantHome)
			}
		})
	}
}

// A folder delivery made in such a namespace is subscribed in the namespace the
// recipient's sessions read, not in a root the variables left empty (#2605).
func TestDeliverySubscribesWhereASessionReads(t *testing.T) {
	root := t.TempDir()
	s := &session{opts: testOpts(Options{
		Mailbox: maildir.New(),
		Index:   fileindex.New(),
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/"},
			{Type: "shared", Prefix: "Team/", Separator: "/", Location: "maildir:%h/team"},
		},
	})}
	personal := &mailbox.UserInfo{Username: "alice@x", Home: filepath.Join(root, "alice"), MailPath: filepath.Join(root, "alice", "Maildir")}
	sub := s.subscriberFor(personal, "Team/Plans")
	if sub == nil {
		t.Fatal("no subscriber for Team/Plans")
	}
	if err := sub("Plans"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	found, err := filepath.Glob(filepath.Join(root, "alice", "team", "subscriptions*"))
	if err != nil || len(found) == 0 {
		t.Errorf("no subscription file under the recipient's team root: %v %v", found, err)
	}
}
