package imap_test

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/v2/internal/imap"
	fileindex "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/internal/storage/nsopen"
	"github.com/yarilomail/yarilo/v2/internal/storage/virtualsrc"
	"github.com/yarilomail/yarilo/v2/internal/userstate/acl"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The user's rights on the public folders: News read only, Desk everything,
// Board flags but no expunge, Secret nothing. other@test.com may post to News.
var publicRights = []config.GlobalACLRule{
	{Mailbox: "News", Entries: []config.GlobalACLEntry{
		{Identifier: "user=user@test.com", Rights: "lr"}, {Identifier: "user=other@test.com", Rights: "lrsi"},
	}},
	{Mailbox: "Desk", Entries: []config.GlobalACLEntry{{Identifier: "user=user@test.com", Rights: "lrswipkxte"}}},
	{Mailbox: "Board", Entries: []config.GlobalACLEntry{{Identifier: "user=user@test.com", Rights: "lrswt"}}},
}

// publicVirtualServer is virtualServer with a public namespace beside the
// personal one, its folders seeded by seedPublic, and the virtual driver
// assembled as the binaries assemble it, through virtualsrc (#2603).
func publicVirtualServer(t *testing.T, configs map[string]string, seed, seedPublic func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex), tunes ...func(*imapserver.Options)) (net.Conn, *bufio.Reader) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	info, _ := resolver.UserInfo("user@test.com", "")
	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	ui := fileindex.New().OpenUser(info)
	seed(t, box, ui)
	ui.Close()  //nolint:errcheck
	box.Close() //nolint:errcheck

	archLoc := "maildir:" + filepath.Join(info.Home, "archive")
	arch := nsopen.Namespace{Type: "personal", Prefix: "Archive/", Separator: "/", Location: archLoc}
	archUI, _, err := nsopen.Fixed(arch, info, info, mailbox.UserStore)
	if err != nil {
		t.Fatal(err)
	}
	abox := maildir.New().OpenUser(archUI)
	if err := abox.Init(); err != nil {
		t.Fatal(err)
	}
	aui := fileindex.New().OpenUser(archUI)
	saveInto(t, abox, aui, "2025", 1, "archived", nil)
	aui.Close()  //nolint:errcheck
	abox.Close() //nolint:errcheck

	pubLoc := "maildir:" + filepath.Join(root, "public")
	pub := nsopen.Namespace{Type: "shared", Prefix: "Public/", Separator: "/", Location: pubLoc}
	pubUI, _, err := nsopen.Fixed(pub, info, info, mailbox.SharedByAll)
	if err != nil {
		t.Fatal(err)
	}
	pbox := maildir.New().OpenUser(pubUI)
	if err := pbox.Init(); err != nil {
		t.Fatal(err)
	}
	pui := fileindex.New().OpenUser(pubUI)
	seedPublic(t, pbox, pui)
	pui.Close()  //nolint:errcheck
	pbox.Close() //nolint:errcheck

	for name, text := range configs {
		dir := filepath.Join(info.Home, "virtual", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, virtual.ConfigFileName), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	global, err := acl.NewGlobal(publicRights)
	if err != nil {
		t.Fatal(err)
	}
	cfgNS := []config.NamespaceConfig{
		{Type: "personal", Prefix: "", Separator: "/"},
		{Type: "shared", Prefix: "Public/", Separator: "/", Location: pubLoc},
		{Type: "shared", Prefix: "Other/", Separator: "/"},
		{Type: "personal", Prefix: "Archive/", Separator: "/", Location: archLoc},
		{Type: "personal", Prefix: "Virtual/", Separator: "/", Location: "virtual:%h/virtual"},
	}
	opts := imapserver.Options{
		Mailbox: maildir.New(), Index: fileindex.New(), Resolver: resolver,
		ACLEnabled: true, ACLGlobal: global,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "Public/", Separator: '/', List: imapserver.ListYes, Location: pubLoc},
			{Type: imapserver.NamespaceShared, Prefix: "Other/", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespacePersonal, Prefix: "Archive/", Separator: '/', List: imapserver.ListYes, Location: archLoc},
			{Type: imapserver.NamespacePersonal, Prefix: "Virtual/", Separator: '/', List: imapserver.ListYes, Location: "virtual:%h/virtual"},
		},
		AuthRelay: authtest.RelayTo(t, &enforcePassdb{users: map[string]string{"user@test.com": "testpass", "other@test.com": "testpass"}}),
	}
	for _, tune := range tunes {
		tune(&opts)
	}
	md, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	opts.MetadataDict = md
	mb, idx := opts.Mailbox, opts.Index
	opts.NamespaceMailboxes = map[string]mailbox.MailboxBackend{"Virtual/": virtual.New(virtual.Options{
		Personal: func(ui *mailbox.UserInfo) mailbox.Box {
			return mailboxbase.Open(mb.OpenUser(ui), idx.OpenUser(ui), mailboxbase.ReadOnly())
		},
		Shared: countOpens(virtualsrc.Sources(cfgNS, virtualsrc.Deps{
			Backends: nsopen.Backends{Mailbox: mb}, Index: idx,
			ACL: config.ACLConfig{Enabled: true, Global: publicRights},
		})),
		Declared:     virtualsrc.Declared(cfgNS),
		MetadataDict: md,
		Locker:       opts.Locker,
	})}
	srv := imapserver.New(opts)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	lastPublicAddr = ln.Addr().String()
	return loginTo(t, lastPublicAddr)
}

// namespaceOpens counts, per namespace, how often the virtual driver opened its
// stores on the server publicVirtualServer started last.
var namespaceOpens map[string]int

func countOpens(open func(*mailbox.UserInfo, string) []virtual.Source) func(*mailbox.UserInfo, string) []virtual.Source {
	namespaceOpens = map[string]int{}
	return func(ui *mailbox.UserInfo, ns string) []virtual.Source {
		namespaceOpens[ns]++
		return open(ui, ns)
	}
}

// lastPublicAddr is the server publicVirtualServer started last.
var lastPublicAddr string

func loginAs(t *testing.T, addr, user string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	rd := bufio.NewReader(conn)
	readUntilTag(t, rd, "")
	command(t, conn, rd, "l1", "LOGIN "+user+" testpass")
	return conn, rd
}

// status is the tagged reply of one command, OK or not.
func status(t *testing.T, conn net.Conn, rd *bufio.Reader, tag, cmd string) string {
	t.Helper()
	lines := tagged(t, conn, rd, tag, cmd)
	return lines[len(lines)-1]
}

func personalInbox(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
	saveInto(t, box, ui, "INBOX", 1, "personal", nil)
}

func publicFolders(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
	saveInto(t, box, ui, "News", 1, "news one", nil)
	saveInto(t, box, ui, "News", 2, "news two", nil)
	saveInto(t, box, ui, "Desk", 1, "desk", nil)
	saveInto(t, box, ui, "Secret", 1, "secret", nil)
	saveInto(t, box, ui, "Board", 1, "board", nil)
}

// A virtual mailbox draws from a public folder its pattern names, and "*" does
// not reach the public namespace (#2603 rules 1 and 5).
func TestAVirtualMailboxDrawsFromThePublicNamespace(t *testing.T) {
	conn, rd := publicVirtualServer(t, map[string]string{
		"Mine": "*\n",
		"News": "Public/News\n",
		"Pub":  "Public/*\n",
		"Gone": "Public/Nowhere\nPublic/News\n",
	}, personalInbox, publicFolders)
	for _, tc := range []struct {
		box  string
		want int
	}{
		{"Virtual/Mine", 1}, // the personal INBOX alone
		{"Virtual/News", 2},
		{"Virtual/Pub", 4},  // News, Desk and Board; Secret the user may not read
		{"Virtual/Gone", 2}, // a named folder that does not exist is skipped (rule 4)
	} {
		if got := existsCount(t, conn, rd, "s"+tc.box[8:], tc.box); got != tc.want {
			t.Errorf("%s EXISTS = %d, want %d", tc.box, got, tc.want)
		}
	}
}

// A folder named explicitly that the user may not read fails the mailbox; a
// pattern under a namespace with no store too (#2603 rules 3 and 5).
func TestAVirtualMailboxNamingAnUnreadableFolderFails(t *testing.T) {
	conn, rd := publicVirtualServer(t, map[string]string{
		"Secret": "Public/Secret\n",
		"Other":  "Other/News\n",
	}, personalInbox, publicFolders)
	for _, box := range []string{"Virtual/Secret", "Virtual/Other"} {
		if line := status(t, conn, rd, "x", "SELECT "+box); !strings.Contains(line, " NO") {
			t.Errorf("SELECT %s = %q, want NO", box, line)
		}
	}
}

// A user who may only read Public/News changes nothing there through a virtual
// mailbox; where they hold the rights, the change reaches the copy (#2603 rule 7).
func TestAVirtualMailboxWritesOnlyWhereTheBackingFolderAllows(t *testing.T) {
	conn, rd := publicVirtualServer(t, map[string]string{"News": "Public/News\n", "Desk": "Public/Desk\n"},
		personalInbox, publicFolders)
	if got := existsCount(t, conn, rd, "a2", "Virtual/News"); got != 2 {
		t.Fatalf("Virtual/News EXISTS = %d, want 2", got)
	}
	for _, cmd := range []string{`STORE 1 +FLAGS (\Flagged)`, `STORE 1 +FLAGS (\Deleted)`} {
		if line := status(t, conn, rd, "w", cmd); !strings.Contains(line, " NO") {
			t.Errorf("%s on a read-only backing folder = %q, want NO", cmd, line)
		}
	}
	if got := existsCount(t, conn, rd, "a3", "Public/News"); got != 2 {
		t.Fatalf("Public/News EXISTS = %d, want 2", got)
	}
	if f := flagsOf(t, fetchLine(t, conn, rd, "a4", "FETCH 1 (FLAGS)")); strings.Contains(f, `\Flagged`) || strings.Contains(f, `\Deleted`) {
		t.Errorf("Public/News 1 has FLAGS (%s) after a refused STORE", f)
	}
	if got := existsCount(t, conn, rd, "a5", "Virtual/Desk"); got != 1 {
		t.Fatalf("Virtual/Desk EXISTS = %d, want 1", got)
	}
	command(t, conn, rd, "a6", `STORE 1 +FLAGS (\Flagged)`)
	if got := existsCount(t, conn, rd, "a7", "Public/Desk"); got != 1 {
		t.Fatalf("Public/Desk EXISTS = %d", got)
	}
	if f := flagsOf(t, fetchLine(t, conn, rd, "a8", "FETCH 1 (FLAGS)")); !strings.Contains(f, `\Flagged`) {
		t.Errorf("Public/Desk 1 has FLAGS (%s), want \\Flagged through the virtual mailbox", f)
	}
}

// EXPUNGE through a virtual mailbox needs the expunge right on the backing
// folder: Board lets the user mark a message, not remove it (#2603 rule 7).
func TestAVirtualExpungeNeedsTheBackingFoldersRight(t *testing.T) {
	conn, rd := publicVirtualServer(t, map[string]string{"Board": "Public/Board\n"}, personalInbox, publicFolders)
	if got := existsCount(t, conn, rd, "a2", "Virtual/Board"); got != 1 {
		t.Fatalf("Virtual/Board EXISTS = %d, want 1", got)
	}
	command(t, conn, rd, "a3", `STORE 1 +FLAGS (\Deleted)`)
	if line := status(t, conn, rd, "a4", "EXPUNGE"); !strings.Contains(line, " NO") {
		t.Errorf("EXPUNGE without the backing folder's expunge right = %q, want NO", line)
	}
	if got := existsCount(t, conn, rd, "a5", "Public/Board"); got != 1 {
		t.Errorf("Public/Board EXISTS = %d after a refused EXPUNGE, want 1", got)
	}
}

// An annotation line selects a public folder by its METADATA as it selects a
// personal one (#2603 rule 6).
func TestAnAnnotationLineSelectsAPublicFolder(t *testing.T) {
	conn, rd := publicVirtualServer(t, map[string]string{"Tagged": "Public/*\n/shared/comment:keep*\n"}, personalInbox, publicFolders)
	command(t, conn, rd, "m1", `SETMETADATA Public/Desk (/shared/comment "keep this")`)
	command(t, conn, rd, "m2", `SETMETADATA Public/Board (/shared/comment "drop")`)
	if got := existsCount(t, conn, rd, "a1", "Virtual/Tagged"); got != 1 {
		t.Fatalf("Virtual/Tagged EXISTS = %d, want Public/Desk alone", got)
	}
	if s := subjectOf(t, command(t, conn, rd, "a2", "FETCH 1 (BODY.PEEK[HEADER.FIELDS (SUBJECT)])")); s != "desk" {
		t.Errorf("Virtual/Tagged holds %q, want the message in Public/Desk", s)
	}
}

// A private annotation on a public folder is the user's own, keyed by them: the
// line reads the key GETMETADATA reads, not a personal folder's.
func TestAPrivateAnnotationLineSelectsAPublicFolder(t *testing.T) {
	conn, rd := publicVirtualServer(t, map[string]string{"Mine": "Public/*\n/private/comment:mine*\n"}, personalInbox, publicFolders)
	command(t, conn, rd, "m1", `SETMETADATA Public/Board (/private/comment "mine")`)
	if got := existsCount(t, conn, rd, "a1", "Virtual/Mine"); got != 1 {
		t.Fatalf("Virtual/Mine EXISTS = %d, want Public/Board alone", got)
	}
}

// IDLE on a virtual mailbox over a public folder hears another user's delivery
// there: the folder's events are the shared store's (#2601, #2602; #2603 rule 8).
func TestIdleOnAVirtualMailboxHearsAnotherUserInAPublicFolder(t *testing.T) {
	conn, rd := publicVirtualServer(t, map[string]string{"News": "Public/News\n"}, personalInbox, publicFolders, virtualLocks(t))
	if got := existsCount(t, conn, rd, "a2", "Virtual/News"); got != 2 {
		t.Fatalf("Virtual/News EXISTS = %d, want 2", got)
	}
	other, ord := loginAs(t, lastPublicAddr, "other@test.com")
	fmt.Fprintf(conn, "a3 IDLE\r\n")
	waitFor(t, conn, rd, "+ ", 2*time.Second)
	appendRaw(t, other, ord, "b2", "Public/News", "from another user")
	if line := waitFor(t, conn, rd, " EXISTS", 3*time.Second); line != "* 3 EXISTS" {
		t.Errorf("IDLE answered %q, want * 3 EXISTS", line)
	}
	fmt.Fprintf(conn, "DONE\r\n")
	readTagged(t, rd, "a3")
}

// A virtual mailbox saving to a public folder saves there where the user may
// insert, and is refused where they may only read (#2603 rule 7).
func TestAVirtualSaveIntoAPublicFolderNeedsTheInsertRight(t *testing.T) {
	conn, rd := publicVirtualServer(t, map[string]string{
		"ToDesk": "Public/Desk\n!Public/Desk\n",
		"ToNews": "Public/News\n!Public/News\n",
	}, personalInbox, publicFolders)
	raw := "Subject: saved\r\nFrom: a@test\r\n\r\nbody\r\n"
	for _, tc := range []struct {
		box  string
		want string
	}{{"Virtual/ToDesk", " OK"}, {"Virtual/ToNews", " NO"}} {
		fmt.Fprintf(conn, "s1 APPEND %s {%d+}\r\n%s\r\n", tc.box, len(raw), raw)
		if line := readTagged(t, rd, "s1"); !strings.Contains(line, tc.want) {
			t.Errorf("APPEND %s = %q, want%s", tc.box, line, tc.want)
		}
	}
	if got := existsCount(t, conn, rd, "a2", "Public/Desk"); got != 2 {
		t.Errorf("Public/Desk EXISTS = %d, want the saved message beside the old one", got)
	}
	if got := existsCount(t, conn, rd, "a3", "Public/News"); got != 2 {
		t.Errorf("Public/News EXISTS = %d, want nothing saved", got)
	}
}

// A virtual mailbox opens only the namespaces its lines bind to, once for the
// session: "*" never opens Public/, however often it is polled (#2609 review).
func TestAVirtualMailboxOpensOnlyTheNamespacesItsLinesName(t *testing.T) {
	conn, rd := publicVirtualServer(t, map[string]string{"Mine": "*\n", "Pub": "Public/*\n"}, personalInbox, publicFolders)
	if got := existsCount(t, conn, rd, "a1", "Virtual/Mine"); got != 1 {
		t.Fatalf("Virtual/Mine EXISTS = %d, want 1", got)
	}
	for i := 0; i < 3; i++ {
		command(t, conn, rd, fmt.Sprintf("n%d", i), "NOOP")
	}
	if n := namespaceOpens["Public/"]; n != 0 {
		t.Errorf("Public/ opened %d times for a mailbox no line binds to it, want 0", n)
	}
	if got := existsCount(t, conn, rd, "a2", "Virtual/Pub"); got != 4 {
		t.Fatalf("Virtual/Pub EXISTS = %d, want 4", got)
	}
	for i := 0; i < 3; i++ {
		command(t, conn, rd, fmt.Sprintf("p%d", i), "NOOP")
	}
	if n := namespaceOpens["Public/"]; n != 1 {
		t.Errorf("Public/ opened %d times over a SELECT and three polls, want once", n)
	}
}

// A second personal namespace with a store of its own is drawn from like any
// other namespace (#2603 rule 1, #2609 review).
func TestAVirtualMailboxDrawsFromASecondPersonalStore(t *testing.T) {
	conn, rd := publicVirtualServer(t, map[string]string{"Old": "Archive/*\n"}, personalInbox, publicFolders)
	if got := existsCount(t, conn, rd, "a1", "Virtual/Old"); got != 1 {
		t.Fatalf("Virtual/Old EXISTS = %d, want the message in Archive/2025", got)
	}
	if s := subjectOf(t, command(t, conn, rd, "a2", "FETCH 1 (BODY.PEEK[HEADER.FIELDS (SUBJECT)])")); s != "archived" {
		t.Errorf("Virtual/Old holds %q, want the archived message", s)
	}
}
