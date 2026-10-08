package lmtp

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A recipient whose name would be a path is no such user, and nothing is
// made for it anywhere.
func TestARecipientCannotNameAPath(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "mail")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := New(testOpts(Options{
		Hostname: "lmtp.test",
		Config:   config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	for _, rcpt := range []string{`"../../escape"@example.com`, `"a/b"@example.com`, `".."@example.com`} {
		conn, sc := dialLMTP(t, ln.Addr().String())
		sendLHLO(t, conn, sc)
		fmt.Fprintf(conn, "MAIL FROM:<s@x>\r\n")
		sc.Scan()
		fmt.Fprintf(conn, "RCPT TO:<%s>\r\n", rcpt)
		sc.Scan()
		if got := sc.Text(); !strings.HasPrefix(got, "550 5.1.1") {
			t.Errorf("RCPT %s = %q, want 550 5.1.1", rcpt, got)
		}
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 1 {
		t.Fatalf("the mail root's parent has %d entries, want only the root", len(entries))
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("the mail root has %d entries, want none", len(entries))
	}
}
