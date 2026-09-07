package lmtp

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A folder no session has opened sums records that say no size as nothing, and
// the delivery went into a mailbox already over its limit (#1728).
func TestADeliveryIntoAnUnopenedFolderJudgesTheMail(t *testing.T) {
	dir := t.TempDir()
	resolver := &mailbox.Resolver{
		Root: dir, HomeTemplate: "%d/%n",
		DefaultQuotaRules: []string{"*:storage=4000"},
	}
	info := resolver.UserInfo("alice@example.com", "")
	info.Driver = "maildir"
	mb := maildir.New()
	box := mb.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	idx := fileindex.New().OpenUser(info)
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	// The mail already there is over the limit on its own; the message being
	// delivered is not. So only the sum of what is stored can reject it.
	body := strings.Repeat("x", 4096)
	name := "1700000001.M1P1_1.host,S=4096,W=4096:2,"
	if werr := os.WriteFile(filepath.Join(info.Home, "Maildir", "cur", name), []byte(body), 0o600); werr != nil {
		t.Fatal(werr)
	}
	if aerr := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1}); aerr != nil {
		t.Fatal(aerr)
	}
	if _, aerr := mailbox.Driver(box).(mailbox.UIDNamer).AssignUID("INBOX", name, 1); aerr != nil {
		t.Fatal(aerr)
	}
	box.Close() //nolint:errcheck
	idx.Close() //nolint:errcheck

	srv := New(Options{
		Hostname:    "lmtp.test",
		Config:      config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
		Mailbox:     mb,
		Index:       fileindex.New(),
		QuotaEngine: true,
		Resolver:    resolver,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() { _ = srv.Serve(ln) }()

	conn, sc := dialLMTP(t, ln.Addr().String())
	sendLHLO(t, conn, sc)
	resp := deliver(t, conn, sc, "sender@external.com", "alice@example.com", testMsg)
	if len(resp) == 0 || !strings.HasPrefix(resp[0], "452") {
		t.Fatalf("the delivery was accepted into a full mailbox: %v", resp)
	}
}
