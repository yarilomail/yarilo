package imap_test

import (
	"io/fs"
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

// indexFilesUnder lists every index file in the tree, relative to it.
func indexFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".index") {
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The namespace's own index path decides where its indexes land on disk. It is
// what lets one read-only store be shared: the writes go somewhere writable.
func TestANamespaceIndexPathMovesTheIndexOnDisk(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "vhosts", "shared")
	for _, d := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(store, ".Tucked", d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	idx := filepath.Join(root, "indexes", "shared")

	srv := imapserver.New(imapserver.Options{
		Mailbox:   maildir.New(),
		Index:     file.New(),
		Resolver:  &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		AuthRelay: authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "Shared/", Separator: '/',
				Location: "maildir:" + store + ":INDEX=" + idx, List: imapserver.ListYes},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go srv.Serve(ln)                 //nolint:errcheck

	conn, rd := loginTo(t, ln.Addr().String())
	command(t, conn, rd, "a1", `SELECT "Shared/Tucked"`)

	if got := indexFilesUnder(t, idx); len(got) == 0 {
		t.Errorf("the namespace's index path holds no index; the key changed nothing on disk")
	}
	if got := indexFilesUnder(t, store); len(got) != 0 {
		t.Errorf("indexes were written into the store the namespace only reads: %v", got)
	}
}
