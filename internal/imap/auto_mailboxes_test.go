package imap_test

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// autoStand is a server whose personal namespace configures mailboxes, and a
// shared one at a fixed location that may configure its own.
type autoStand struct {
	addr, root, shared string
}

func startAutoStand(t *testing.T, personal, shared map[string]mailbox.AutoMailbox) autoStand {
	t.Helper()
	st := autoStand{root: t.TempDir(), shared: t.TempDir()}
	srv := imapserver.New(imapserver.Options{
		Mailbox:            maildir.New(),
		Index:              file.New(),
		Resolver:           &mailbox.Resolver{Root: st.root, HomeTemplate: "%d/%n"},
		AuthRelay:          authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
		SpecialUseDefaults: map[string]string{"Sent": `\Sent`},
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes, Mailboxes: personal},
			{Type: imapserver.NamespaceShared, Prefix: "Shared/", Separator: '/', List: imapserver.ListYes,
				Location: "maildir:" + st.shared, Mailboxes: shared},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	st.addr = ln.Addr().String()
	return st
}

func (st autoStand) login(t *testing.T) *imapclient.Client {
	t.Helper()
	conn, err := net.Dial("tcp", st.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c
}

// subsFile is the path of the personal subscription file, found where the
// server put it rather than derived.
func (st autoStand) subsFile(t *testing.T, root, name string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && !d.IsDir() && d.Name() == name {
			found = p
		}
		return nil
	})
	return found
}

func listed(t *testing.T, c *imapclient.Client, opts *imap.ListOptions) map[string]*imap.ListData {
	t.Helper()
	items, err := c.List("", "*", opts).Collect()
	if err != nil {
		t.Fatalf("LIST: %v", err)
	}
	out := map[string]*imap.ListData{}
	for _, it := range items {
		out[it.Mailbox] = it
	}
	return out
}

func subscribedNames(t *testing.T, c *imapclient.Client) []string {
	t.Helper()
	var out []string
	for name, it := range listed(t, c, &imap.ListOptions{SelectSubscribed: true, ReturnSubscribed: true}) {
		if slices.Contains(it.Attrs, imap.MailboxAttrSubscribed) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

var (
	autoSent    = map[string]mailbox.AutoMailbox{"Sent": {Auto: mailbox.AutoSubscribe}}
	autoArchive = map[string]mailbox.AutoMailbox{"Archive2": {Auto: mailbox.AutoCreate}}
)

// auto: create makes the folder on the first LIST, with an identity, and
// subscribes nothing.
func TestAnAutoCreateMailboxIsMadeOnListNotSubscribed(t *testing.T) {
	st := startAutoStand(t, autoArchive, nil)
	c := st.login(t)
	if _, ok := listed(t, c, nil)["Archive2"]; !ok {
		t.Fatal("the first LIST does not show Archive2")
	}
	// Its index, which holds the GUID, is written by the LIST itself.
	if _, err := os.Stat(filepath.Join(st.root, "test.com", "user", ".Archive2", "yarilo.index")); err != nil {
		t.Fatalf("Archive2 has no index after the LIST: %v", err)
	}
	if subs := subscribedNames(t, c); len(subs) != 0 {
		t.Fatalf("auto: create subscribed %v", subs)
	}
}

// auto: subscribe makes and subscribes once: the file holds exactly the one
// line, sorted, no header.
func TestAnAutoSubscribeMailboxIsWrittenOnceToTheFile(t *testing.T) {
	st := startAutoStand(t, autoSent, nil)
	c := st.login(t)
	if _, ok := listed(t, c, nil)["Sent"]; !ok {
		t.Fatal("the first LIST does not show Sent")
	}
	listed(t, c, nil)
	raw, err := os.ReadFile(st.subsFile(t, st.root, "subscriptions"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "Sent\n" {
		t.Fatalf("subscriptions holds %q, want exactly %q", raw, "Sent\n")
	}
}

// An unsubscribe stays: neither a later LIST nor a SELECT puts it back.
func TestAnUnsubscribeOfAnAutoMailboxStays(t *testing.T) {
	for _, then := range []string{"list", "select"} {
		t.Run(then, func(t *testing.T) {
			st := startAutoStand(t, autoSent, nil)
			c := st.login(t)
			listed(t, c, nil)
			if err := c.Unsubscribe("Sent").Wait(); err != nil {
				t.Fatal(err)
			}
			switch then {
			case "list":
				listed(t, c, nil)
			case "select":
				if _, err := c.Select("Sent", nil).Wait(); err != nil {
					t.Fatal(err)
				}
			}
			if subs := subscribedNames(t, c); len(subs) != 0 {
				t.Fatalf("after UNSUBSCRIBE and %s the subscribed set is %v", then, subs)
			}
			lsub := listed(t, c, &imap.ListOptions{SelectSubscribed: true, ReturnSubscribed: true})
			if _, ok := lsub["Sent"]; ok {
				t.Fatal("Sent is back among the subscribed")
			}
		})
	}
}

// A folder the user made first is theirs: no subscription is added, and the
// configured special use still applies.
func TestAUserMadeFolderOfTheSameNameKeepsItsSubscription(t *testing.T) {
	st := startAutoStand(t, autoSent, nil)
	c := st.login(t)
	if err := c.Create("Sent", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	got := listed(t, c, nil)["Sent"]
	if got == nil || !slices.Contains(got.Attrs, imap.MailboxAttrSent) {
		t.Fatalf("Sent lists as %+v, want it with \\Sent", got)
	}
	if subs := subscribedNames(t, c); len(subs) != 0 {
		t.Fatalf("a user-made Sent was subscribed: %v", subs)
	}
}

// A client may SELECT or STATUS the name before it lists anything.
func TestSelectOrStatusMakesAnAutoMailbox(t *testing.T) {
	for _, cmd := range []string{"select", "status"} {
		t.Run(cmd, func(t *testing.T) {
			st := startAutoStand(t, autoSent, nil)
			c := st.login(t)
			var err error
			if cmd == "select" {
				_, err = c.Select("Sent", nil).Wait()
			} else {
				_, err = c.Status("Sent", &imap.StatusOptions{NumMessages: true}).Wait()
			}
			if err != nil {
				t.Fatalf("%s of an auto mailbox before any LIST: %v", cmd, err)
			}
			if subs := subscribedNames(t, c); !slices.Equal(subs, []string{"Sent"}) {
				t.Fatalf("subscribed %v after %s, want [Sent]", subs, cmd)
			}
		})
	}
}

// A namespace with its own store makes its mailbox there, and subscribes it in
// its own file under the relative name.
func TestASharedNamespaceMakesItsAutoMailboxInItsStore(t *testing.T) {
	st := startAutoStand(t, nil, map[string]mailbox.AutoMailbox{"Board": {Auto: mailbox.AutoSubscribe}})
	c := st.login(t)
	if _, ok := listed(t, c, nil)["Shared/Board"]; !ok {
		t.Fatal("the first LIST does not show Shared/Board")
	}
	if _, err := os.Stat(filepath.Join(st.shared, ".Board")); err != nil {
		t.Fatalf("Board is not in the shared store: %v", err)
	}
	if subs := subscribedNames(t, c); !slices.Equal(subs, []string{"Shared/Board"}) {
		t.Fatalf("subscribed %v, want [Shared/Board]", subs)
	}
}

// Another implementation's version-2 file in our file's place is not converted
// by a subscription the server adds on its own; the folder is still made.
func TestAnAutoSubscribeLeavesAForeignSubscriptionFileAsItIs(t *testing.T) {
	st := startAutoStand(t, autoSent, nil)
	home := filepath.Join(st.root, "test.com", "user")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("V\t2\n\nArchive\n")
	path := filepath.Join(home, "subscriptions")
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	c := st.login(t)
	if _, ok := listed(t, c, nil)["Sent"]; !ok {
		t.Fatal("the first LIST does not show Sent")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(foreign) {
		t.Fatalf("the foreign file was rewritten: %q", raw)
	}
}
