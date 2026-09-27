package imap_test

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// hiddenNSServer serves a hidden namespace beside the personal one.
func hiddenNSServer(t *testing.T) (net.Conn, *bufio.Reader) {
	t.Helper()
	root := t.TempDir()
	shared := filepath.Join(root, "vhosts", "hidden")
	if err := os.MkdirAll(filepath.Join(shared, ".Tucked", "cur"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(shared, ".Tucked", d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	srv := imapserver.New(imapserver.Options{
		Mailbox:   maildir.New(),
		Index:     file.New(),
		Resolver:  &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		AuthRelay: authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "Hidden/", Separator: '/',
				Location: "maildir:" + shared, List: imapserver.ListYes, Hidden: true},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go srv.Serve(ln)                 //nolint:errcheck
	return loginTo(t, ln.Addr().String())
}

// A hidden namespace stays out of a wildcard LIST and answers when it is named
// (RFC 2342): that is what the key is for, and it did nothing before (#2071).
func TestAHiddenNamespaceIsListedOnlyWhenNamed(t *testing.T) {
	conn, rd := hiddenNSServer(t)
	wild := strings.Join(command(t, conn, rd, "a1", `LIST "" "*"`), "\n")
	if strings.Contains(wild, "Hidden/") {
		t.Errorf(`LIST "" "*" answered with the hidden namespace:\n%s`, wild)
	}
	if !strings.Contains(wild, "INBOX") {
		t.Errorf("the personal namespace went missing too:\n%s", wild)
	}
	named := strings.Join(command(t, conn, rd, "a2", `LIST "" "Hidden/*"`), "\n")
	if !strings.Contains(named, "Hidden/Tucked") {
		t.Errorf("named exactly, the namespace answered nothing:\n%s", named)
	}
}
