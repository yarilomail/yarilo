package imap_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// startIdentityServer is the raw-connection server with an annotation dict, so
// METADATA reaches the resolve that reads a folder's GUID: without a dict the
// command answers before it ever opens the folder.
func startIdentityServer(t *testing.T) (root, addr string) {
	t.Helper()
	root = t.TempDir()
	md, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	srv := imapserver.New(imapserver.Options{
		Mailbox:      maildir.New(),
		Index:        file.New(),
		Resolver:     &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		AuthRelay:    authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
		MetadataDict: md,
		// The engine and a per-mailbox message cap, so a save reaches the
		// per-folder count: without them that caller is never called.
		QuotaEngine: true,
		QuotaPolicy: quota.Policy{MailboxMessageCount: 1000},
	})
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatal(lerr)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	return root, ln.Addr().String()
}

func storeWalks(t *testing.T) float64 {
	t.Helper()
	return testutil.ToFloat64(mailboxbase.MetricReconcile.WithLabelValues("scanned")) +
		testutil.ToFloat64(mailboxbase.MetricReconcile.WithLabelValues("scanned-untokened"))
}

// changeOutOfBand drops a file into a folder's cur/ the way another MUA does,
// and backdates the directory so the change is the moved mtime and not the
// same-second rule.
func changeOutOfBand(t *testing.T, root, folder, name string) {
	t.Helper()
	dir := folderDir(root, folder)
	if err := os.WriteFile(filepath.Join(dir, "cur", name), []byte("Subject: out of band\r\n\r\nx\r\n"), 0o600); err != nil {
		t.Fatalf("out-of-band write: %v", err)
	}
	backdate(t, dir)
}

// settleFolder puts a folder's directories far enough in the past that their
// mtime stands for their contents.
func settleFolder(t *testing.T, root, folder string) {
	t.Helper()
	backdate(t, folderDir(root, folder))
}

func backdate(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	for _, sub := range []string{"cur", "new"} {
		if err := os.Chtimes(filepath.Join(dir, sub), old, old); err != nil {
			t.Fatalf("backdate %s: %v", sub, err)
		}
	}
}

func folderDir(root, folder string) string {
	dir := filepath.Join(root, "test.com", "user", "Maildir")
	if folder != "INBOX" {
		dir = filepath.Join(dir, "."+folder)
	}
	return dir
}

// Four callers asked a session box for a folder and got a store walk with it,
// while needing only the folder's identity and the index: METADATA's GUID, the
// per-folder quota count, and the destination of APPEND/COPY/MOVE, which
// writes its own record (#1875).
func TestIdentityOnlyCallersDoNotWalkTheStore(t *testing.T) {
	root, addr := startIdentityServer(t)
	c := dialRaw(t, addr)
	c.login()
	c.cmd(`CREATE Dest`)
	appendSubject(t, c, "INBOX", "source")
	// The selected folder is settled and left alone: every walk counted below
	// then belongs to the destination, not to the poll that follows a command.
	settleFolder(t, root, "INBOX")
	c.cmd(`SELECT INBOX`)

	tests := []struct {
		name string
		run  func()
	}{
		{name: "METADATA reads a GUID", run: func() { c.cmd(`GETMETADATA Dest (/private/comment)`) }},
		{name: "APPEND writes its own record", run: func() { appendSubject(t, c, "Dest", "appended") }},
		{name: "COPY writes its own record", run: func() { c.cmd(`COPY 1 Dest`) }},
		// MOVE and RENAME INBOX are not here, for one reason: each moves mail
		// out of the selected folder, so the poll that follows walks it for a
		// reason of its own and the destination cannot be told apart. MOVE
		// reaches its destination through the same ensureFolderHandle as
		// APPEND and COPY; the RENAME destination is a folder created one line
		// above the open, with nothing in the store to take.
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The destination is changed behind the session's back, so a
			// settling open would have something to import and the walk would
			// be visible in the counter.
			changeOutOfBand(t, root, "Dest", "1700000400.M1P1.h"+strings.ReplaceAll(tc.name, " ", "")+":2,")
			before := storeWalks(t)
			tc.run()
			if got := storeWalks(t) - before; got != 0 {
				t.Errorf("%s walked the store %v times; it needs the folder's identity, not what the store holds", tc.name, got)
			}
		})
	}
}

// RENAME INBOX moves mail out of the selected folder, so a counter cannot say
// whether the destination was walked too: the source is walked for a reason of
// its own. The seam names each folder as it is walked, which answers it (#1875).
func TestTheRenameDestinationIsNotWalked(t *testing.T) {
	root, addr := startIdentityServer(t)
	c := dialRaw(t, addr)
	c.login()
	appendSubject(t, c, "INBOX", "to be moved")
	settleFolder(t, root, "INBOX")
	c.cmd(`SELECT INBOX`)

	var mu sync.Mutex
	var walked []string
	defer mailboxbase.SetWalkedFolder(func(folder string) {
		mu.Lock()
		defer mu.Unlock()
		walked = append(walked, folder)
	})()

	c.cmd(`RENAME INBOX Moved`)

	mu.Lock()
	defer mu.Unlock()
	for _, folder := range walked {
		if folder == "Moved" {
			t.Errorf("the rename destination was walked; it was created one line above the open and holds nothing the store can add (walked: %v)", walked)
		}
	}
	if len(walked) == 0 {
		t.Fatal("nothing was walked at all, so the seam proves nothing about the destination")
	}
}
