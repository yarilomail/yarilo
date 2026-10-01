package jmap

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A JMAP client's first Mailbox/get on a fresh account, with no IMAP session
// before it, sees the configured mailboxes with their roles and subscriptions.
func TestAFreshAccountShowsTheConfiguredMailboxesOverJMAP(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	locker := &testLocker{}
	s := New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: testLimits(),
		Storage: &Storage{
			Mailbox:            maildir.New(),
			Index:              file.New(file.WithLocker(locker)),
			ResolveUser:        func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:             locker,
			SpecialUseDefaults: map[string]string{"Sent": `\Sent`, "Trash": `\Trash`},
			Mailboxes: map[string]mailbox.AutoMailbox{
				"Sent":  {Auto: mailbox.AutoSubscribe},
				"Trash": {Auto: mailbox.AutoCreate},
			},
		},
	})
	got := callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Mailbox/get",{"accountId":"u1@example.com"},"c0"]]}`)
	seen := map[string]map[string]any{}
	for _, item := range got["list"].([]any) {
		mb := item.(map[string]any)
		seen[mb["name"].(string)] = mb
	}
	for name, want := range map[string]struct {
		role       string
		subscribed bool
	}{"Sent": {"sent", true}, "Trash": {"trash", false}} {
		mb := seen[name]
		if mb == nil {
			t.Fatalf("%s is not listed: %v", name, seen)
		}
		if mb["role"] != want.role || mb["isSubscribed"] != want.subscribed {
			t.Fatalf("%s: role %v subscribed %v, want %s %v", name, mb["role"], mb["isSubscribed"], want.role, want.subscribed)
		}
	}
	raw, err := os.ReadFile(filepath.Join(mailbox.ControlRoot(info), "subscriptions"))
	if err != nil || string(raw) != "Sent\n" {
		t.Fatalf("subscriptions holds %q (%v), want %q", raw, err, "Sent\n")
	}
}
