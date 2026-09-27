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

// twoVisibilityNSServer serves two shared namespaces beside the personal one:
// one hidden from NAMESPACE, one kept out of LIST.
func twoVisibilityNSServer(t *testing.T) (net.Conn, *bufio.Reader) {
	t.Helper()
	root := t.TempDir()
	mk := func(name string) string {
		dir := filepath.Join(root, "vhosts", name)
		for _, d := range []string{"cur", "new", "tmp"} {
			if err := os.MkdirAll(filepath.Join(dir, ".Tucked", d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	unseen, quiet := mk("unseen"), mk("quiet")
	srv := imapserver.New(imapserver.Options{
		Mailbox:   maildir.New(),
		Index:     file.New(),
		Resolver:  &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		AuthRelay: authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "Unseen/", Separator: '/',
				Location: "maildir:" + unseen, List: imapserver.ListYes, Hidden: true},
			{Type: imapserver.NamespaceShared, Prefix: "Quiet/", Separator: '/',
				Location: "maildir:" + quiet, List: imapserver.ListNo},
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

// hidden and list are two settings, not one spelled twice: hidden decides the
// NAMESPACE reply, list decides LIST, and neither reaches into the other.
func TestHiddenAndListDecideDifferentReplies(t *testing.T) {
	conn, rd := twoVisibilityNSServer(t)

	ns := strings.Join(command(t, conn, rd, "a1", "NAMESPACE"), "\n")
	if strings.Contains(ns, "Unseen/") {
		t.Errorf("hidden=yes was advertised in NAMESPACE:\n%s", ns)
	}
	if !strings.Contains(ns, "Quiet/") {
		t.Errorf("list=no was dropped from NAMESPACE, which only hidden may do:\n%s", ns)
	}

	wild := strings.Join(command(t, conn, rd, "a2", `LIST "" "*"`), "\n")
	if !strings.Contains(wild, "Unseen/Tucked") {
		t.Errorf("hidden=yes was kept out of a wildcard LIST, which is list's job:\n%s", wild)
	}
	if strings.Contains(wild, "Quiet/") {
		t.Errorf("list=no answered a wildcard LIST:\n%s", wild)
	}

	named := strings.Join(command(t, conn, rd, "a3", `LIST "" "Quiet/*"`), "\n")
	if !strings.Contains(named, "Quiet/Tucked") {
		t.Errorf("named by its prefix, list=no answered nothing:\n%s", named)
	}
}
