package imap_test

import (
	"bufio"
	"net"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// An INBOX namespace with a location is still the primary and still opens the
// user's own store: its location names the driver, not a second store (#2038).
func TestInboxNamespaceWithALocationIsThePrimary(t *testing.T) {
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	info := resolver.UserInfo("user@test.com", "")
	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	ui := fileindex.New().OpenUser(info)
	saveInto(t, box, ui, "INBOX", 1, "a message in INBOX", nil)
	ui.Close()  //nolint:errcheck
	box.Close() //nolint:errcheck

	srv := imapserver.New(imapserver.Options{
		Mailbox:    maildir.New(),
		Index:      fileindex.New(),
		Resolver:   resolver,
		ACLEnabled: true,
		Namespaces: []imapserver.NamespaceSpec{
			// The virtual one first: the primary is chosen by inbox, not order.
			{Type: imapserver.NamespacePersonal, Prefix: "Virtual/", Separator: '/', List: imapserver.ListYes,
				Location: "virtual:%h/virtual"},
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes,
				Location: "maildir:%h/elsewhere", Inbox: true},
		},
		NamespaceMailboxes: map[string]mailbox.MailboxBackend{"Virtual/": virtual.New()},
		AuthRelay:          authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	rd := bufio.NewReader(conn)
	readUntilTag(t, rd, "")

	command(t, conn, rd, "a1", "LOGIN user@test.com testpass")
	lines := command(t, conn, rd, "a2", "SELECT INBOX")
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "1 EXISTS") {
		t.Errorf("SELECT INBOX answered %q, want the one message the user's own store holds", got)
	}
	for _, line := range command(t, conn, rd, "a3", "NAMESPACE") {
		if strings.HasPrefix(line, "* NAMESPACE") && strings.Contains(line, `("" ".")`) {
			t.Errorf("a second empty-prefix namespace was synthesised: %s", line)
		}
	}
}
