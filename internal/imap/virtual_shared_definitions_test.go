package imap_test

import (
	"bufio"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// treeState is every file under root with its size and modification time, for
// comparing a directory against itself later.
func treeState(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		out = append(out, strings.Join([]string{rel, info.Mode().String(),
			fs.FormatFileInfo(info)}, "|"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// sharedDefsServer gives the user a virtual namespace whose definitions live
// outside the home, in a directory the process may only read.
func sharedDefsServer(t *testing.T, defs map[string]string) (net.Conn, *bufio.Reader, string, string) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	info, _ := resolver.UserInfo("user@test.com", "")

	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ui := fileindex.New().OpenUser(info)
	saveInto(t, box, ui, "INBOX", 1, "first", nil)
	ui.Close()  //nolint:errcheck
	box.Close() //nolint:errcheck

	shared := filepath.Join(root, "definitions")
	for name, text := range defs {
		dir := filepath.Join(shared, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, virtual.ConfigFileName), []byte(text), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	// Read and execute, no write: a shared mount is what this namespace is
	// for, and a write attempt must fail here rather than be tolerated.
	if err := filepath.WalkDir(shared, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		return os.Chmod(p, 0o500)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(shared, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				return os.Chmod(p, 0o700)
			}
			return err
		})
	})

	opts := imapserver.Options{
		Mailbox:    maildir.New(),
		Index:      fileindex.New(),
		Resolver:   resolver,
		ACLEnabled: true,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespacePersonal, Prefix: "Virtual/", Separator: '/', List: imapserver.ListYes,
				Location: "virtual:" + shared + ":INDEX=%h/index/virtual"},
		},
		AuthRelay: authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
	}
	opts.NamespaceMailboxes = map[string]mailbox.MailboxBackend{"Virtual/": virtualDriver(opts)}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	addr := ln.Addr().String()
	lastVirtualAddr = addr
	conn, rd := loginTo(t, addr)
	return conn, rd, shared, info.Home
}

// One definition serves every user: the directory holding it is only read, and
// the uids, the flags and the indexes this session writes land elsewhere.
func TestASharedDefinitionDirectoryIsNeverWrittenTo(t *testing.T) {
	conn, rd, shared, _ := sharedDefsServer(t, map[string]string{"All": "INBOX\n"})
	before := treeState(t, shared)

	if got := existsCount(t, conn, rd, "a1", "Virtual/All"); got != 1 {
		t.Fatalf("EXISTS = %d, want the seeded message", got)
	}

	other, ord := loginTo(t, lastVirtualAddr)
	existsCount(t, other, ord, "b1", "INBOX")
	appendInbox(t, other, ord, "b2", "second")
	if got := linesWith(command(t, conn, rd, "a2", "NOOP"), " EXISTS"); len(got) != 1 || got[0] != "* 2 EXISTS" {
		t.Errorf("NOOP after delivery answered %v, want * 2 EXISTS", got)
	}

	command(t, conn, rd, "a3", `STORE 1 +FLAGS (\Seen)`)
	if f := flagsOf(t, fetchLine(t, conn, rd, "a4", "FETCH 1 (FLAGS)")); !strings.Contains(f, `\Seen`) {
		t.Errorf("the flag did not stick: FLAGS (%s)", f)
	}

	if after := treeState(t, shared); !equalTrees(before, after) {
		t.Errorf("the definition directory was written to.\nbefore: %v\nafter:  %v", before, after)
	}
}

func equalTrees(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A virtual mailbox is not subscribed to. The refusal comes before the name is
// judged and before any write, so a read-only definition directory holds.
func TestAVirtualMailboxIsNotSubscribedTo(t *testing.T) {
	conn, rd, shared, home := sharedDefsServer(t, map[string]string{"All": "INBOX\n"})
	defsBefore, homeBefore := treeState(t, shared), treeState(t, home)

	if answer := last(tagged(t, conn, rd, "a1", `SUBSCRIBE "Virtual/All"`)); !strings.Contains(answer, "NO [CANNOT]") {
		t.Errorf("SUBSCRIBE answered %q, want NO [CANNOT]", answer)
	}
	if got := strings.Join(command(t, conn, rd, "a2", `LIST (SUBSCRIBED) "" "*"`), "\n"); strings.Contains(got, "Virtual/All") {
		t.Errorf("the refused mailbox is listed as subscribed:\n%s", got)
	}
	if after := treeState(t, shared); !equalTrees(defsBefore, after) {
		t.Errorf("the definition directory was written to.\nbefore: %v\nafter:  %v", defsBefore, after)
	}
	if after := treeState(t, home); !equalTrees(homeBefore, after) {
		t.Errorf("the refused subscription reached the user's own state.\nbefore: %v\nafter:  %v", homeBefore, after)
	}
}

// Even where the subscription could be written, it is refused: what stops it is
// the mailbox being virtual, not the directory's permissions.
func TestAVirtualMailboxIsNotSubscribedToEvenWhenWritable(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "one", nil)
		})
	if answer := last(tagged(t, conn, rd, "a2", `SUBSCRIBE "Virtual/All"`)); !strings.Contains(answer, "NO [CANNOT]") {
		t.Errorf("SUBSCRIBE answered %q, want NO [CANNOT]", answer)
	}
	if got := strings.Join(command(t, conn, rd, "a3", `LIST (SUBSCRIBED) "" "*"`), "\n"); strings.Contains(got, "Virtual/All") {
		t.Errorf("the refused mailbox is listed as subscribed:\n%s", got)
	}
	// A folder of the same store still subscribes: the refusal is not a blanket
	// one over the session.
	if answer := last(tagged(t, conn, rd, "a4", `SUBSCRIBE "INBOX"`)); !strings.Contains(answer, "OK") {
		t.Errorf("SUBSCRIBE INBOX answered %q, want OK", answer)
	}
}

// A virtual mailbox has no access rights of its own: the write is refused
// before it reaches a store that may be a shared read-only directory.
func TestAVirtualMailboxTakesNoACLWrite(t *testing.T) {
	conn, rd, shared, home := sharedDefsServer(t, map[string]string{"All": "*\n  all\n"})
	defsBefore, homeBefore := treeState(t, shared), treeState(t, home)

	for _, tc := range []struct{ what, cmd string }{
		{"SETACL", `SETACL "Virtual/All" anyone lr`},
		{"DELETEACL", `DELETEACL "Virtual/All" anyone`},
	} {
		if answer := last(tagged(t, conn, rd, "a"+tc.what, tc.cmd)); !strings.Contains(answer, "NO [CANNOT]") {
			t.Errorf("%s answered %q, want NO [CANNOT]", tc.what, answer)
		}
	}
	// Reads keep working: nothing about them needs the store to be writable.
	for _, tc := range []struct{ what, cmd string }{
		{"GETACL", `GETACL "Virtual/All"`},
		{"MYRIGHTS", `MYRIGHTS "Virtual/All"`},
	} {
		if answer := last(tagged(t, conn, rd, "b"+tc.what, tc.cmd)); !strings.Contains(answer, "OK") {
			t.Errorf("%s answered %q, want OK", tc.what, answer)
		}
	}
	if after := treeState(t, shared); !equalTrees(defsBefore, after) {
		t.Errorf("the definition directory was written to.\nbefore: %v\nafter:  %v", defsBefore, after)
	}
	if after := treeState(t, home); !equalTrees(homeBefore, after) {
		t.Errorf("the refused write reached the user's own state.\nbefore: %v\nafter:  %v", homeBefore, after)
	}
}

// Even where the ACL could be written, it is refused: what stops it is the
// mailbox being virtual, not the directory's permissions.
func TestAVirtualMailboxTakesNoACLWriteEvenWhenWritable(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "one", nil)
		})
	if answer := last(tagged(t, conn, rd, "a2", `SETACL "Virtual/All" anyone lr`)); !strings.Contains(answer, "NO [CANNOT]") {
		t.Errorf("SETACL answered %q, want NO [CANNOT]", answer)
	}
	// A folder of the same store still takes one: the refusal is not a blanket
	// one over the session.
	if answer := last(tagged(t, conn, rd, "a3", `SETACL "INBOX" anyone lr`)); !strings.Contains(answer, "OK") {
		t.Errorf("SETACL INBOX answered %q, want OK", answer)
	}
}
