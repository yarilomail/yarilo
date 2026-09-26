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

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	"github.com/yarilomail/yarilo/internal/fts/language"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbuild"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// virtualServer gives a user an INBOX and an Archive with the messages named,
// and a virtual namespace whose mailboxes are the configurations written here.
func virtualServer(t *testing.T, configs map[string]string, seed func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex)) (net.Conn, *bufio.Reader) {
	t.Helper()
	return virtualServerFTS(t, configs, seed, nil)
}

// virtualServerFTS is the same server with full-text search answered by the
// fake given; nil leaves search off.
func virtualServerFTS(t *testing.T, configs map[string]string, seed func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex), fake *fakeFTS) (net.Conn, *bufio.Reader) {
	t.Helper()
	return virtualServerWith(t, configs, seed, fake, nil)
}

// virtualServerWith lets a row add what the server needs beyond the namespace,
// such as a sieve engine.
func virtualServerWith(t *testing.T, configs map[string]string, seed func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex), fake *fakeFTS, tune func(*imapserver.Options)) (net.Conn, *bufio.Reader) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	info := resolver.UserInfo("user@test.com", "")

	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ui := fileindex.New().OpenUser(info)
	seed(t, box, ui)
	ui.Close()  //nolint:errcheck
	box.Close() //nolint:errcheck

	for name, text := range configs {
		dir := filepath.Join(info.Home, "virtual", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, virtual.ConfigFileName), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	opts := imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    fileindex.New(),
		Resolver: resolver,
		// ACL on and the namespace private, as the stand has them: a row that
		// runs without ACL cannot see a namespace its user does not own.
		ACLEnabled: true,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespacePersonal, Prefix: "Virtual/", Separator: '/', List: imapserver.ListYes,
				Location: "virtual:%h/virtual"},
		},
		NamespaceMailboxes: map[string]mailbox.MailboxBackend{
			"Virtual/": mailboxbuild.ByDriver("virtual", config.StorageConfig{}, nil),
		},
		AuthRelay: authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
	}
	if fake != nil {
		set := language.DefaultSettings()
		chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
		if err != nil {
			t.Fatal(err)
		}
		opts.FTS = imapserver.FTSOptions{
			Client: fake, Chain: chain, AddMissing: "body-search-only", ReadFallback: true,
			Timeout: 300 * time.Millisecond, Autoindex: true, SearchEnabled: true,
		}
	}
	if tune != nil {
		tune(&opts)
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	addr := ln.Addr().String()
	lastVirtualAddr = addr
	return loginTo(t, addr)
}

// lastVirtualAddr is the server virtualServer started last, for a row that
// needs a second session of the same user.
var lastVirtualAddr string

func loginTo(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	rd := bufio.NewReader(conn)
	readUntilTag(t, rd, "")
	command(t, conn, rd, "a1", "LOGIN user@test.com testpass")
	return conn, rd
}

func saveInto(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex, folder string, uid uint32, subject string, flags []string) [16]byte {
	t.Helper()
	return saveCopy(t, box, ui, folder, uid, subject, flags, [16]byte{})
}

// saveCopy files the same message again: with a GUID given, both folders hold
// one message, which is what a copy is.
func saveCopy(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex, folder string, uid uint32, subject string, flags []string, want [16]byte) [16]byte {
	t.Helper()
	if folder != "INBOX" {
		box.Create(folder) //nolint:errcheck
	}
	raw := fmt.Sprintf("Subject: %s\r\nFrom: a@test\r\nDate: Sun, 1 Mar 2026 10:00:00 +0000\r\n\r\nbody\r\n", subject)
	name, vsize, guid, err := box.Save(folder, strings.NewReader(raw), uid, int64(len(raw)), flags, nil, want)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ui.OpenFolder(folder, 0)
	if err != nil {
		t.Fatal(err)
	}
	meta := &mailbox.MessageMeta{
		UID: uid, Size: uint32(len(raw)), VSize: vsize, GUID: guid, Flags: flags,
		InternalDate: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := mailboxbase.NameSaved(box, folder, name, meta); err != nil {
		t.Fatal(err)
	}
	if err := ui.AppendMessage(f.ID, meta); err != nil {
		t.Fatal(err)
	}
	return guid
}

func existsCount(t *testing.T, conn net.Conn, rd *bufio.Reader, tag, mailboxName string) int {
	t.Helper()
	for _, line := range command(t, conn, rd, tag, "SELECT "+mailboxName) {
		if strings.HasSuffix(line, " EXISTS") {
			var n int
			if _, err := fmt.Sscanf(line, "* %d EXISTS", &n); err == nil {
				return n
			}
		}
	}
	return -1
}

// The rule decides membership when the mailbox is synced, so a message it
// does not keep is not in the mailbox: EXISTS says so, without a SEARCH.
func TestVirtualMailboxHoldsOnlyWhatTheRuleKeeps(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"Unseen": "INBOX\n  unseen\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "unread one", nil)
			saveInto(t, box, ui, "INBOX", 2, "read one", []string{`\Seen`})
			saveInto(t, box, ui, "INBOX", 3, "unread two", nil)
		})
	if got := existsCount(t, conn, rd, "a2", "Virtual/Unseen"); got != 2 {
		t.Errorf("the mailbox holds %d messages, want the 2 the rule keeps", got)
	}
}

// One message in two folders is two messages here: the same GUID in both is
// what makes it a copy rather than two messages.
func TestVirtualMailboxListsBothCopies(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\nArchive\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			guid := saveInto(t, box, ui, "INBOX", 1, "one message", nil)
			saveCopy(t, box, ui, "Archive", 1, "one message", []string{`\Seen`}, guid)
		})
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d, want 2: a copy in two folders is two messages here", got)
	}
	uids := map[string]bool{}
	for _, line := range command(t, conn, rd, "a3", "FETCH 1:* (UID)") {
		if strings.Contains(line, "FETCH") {
			uids[line] = true
		}
	}
	if len(uids) != 2 {
		t.Errorf("FETCH answered %d rows, want one per copy: %v", len(uids), uids)
	}
}

// A client cannot make a virtual mailbox: the configuration file is what
// defines one, so CREATE would leave a mailbox with no rule.
func TestCreateInTheVirtualNamespaceIsRefused(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "a message", nil)
		})
	fmt.Fprintf(conn, "a2 CREATE Virtual/Invented\r\n")
	last := readTagged(t, rd, "a2")
	if !strings.Contains(last, "NO [CANNOT]") || !strings.Contains(last, "configuration file") {
		t.Errorf("CREATE answered %q, want the namespace's own refusal", last)
	}
}

// New mail in a backing folder appears without reopening the client's view of
// the world: the next SELECT syncs it.
func TestVirtualMailboxPicksUpNewMail(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "first", nil)
		})
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1", got)
	}
	// New mail in the backing folder, through the same session.
	raw := "Subject: second\r\nFrom: a@test\r\n\r\nbody\r\n"
	fmt.Fprintf(conn, "a3 APPEND INBOX {%d+}\r\n%s\r\n", len(raw), raw)
	if line := readTagged(t, rd, "a3"); !strings.Contains(line, "OK") {
		t.Fatalf("APPEND answered %q", line)
	}
	if got := existsCount(t, conn, rd, "a4", "Virtual/All"); got != 2 {
		t.Errorf("EXISTS = %d after new mail, want 2: the next SELECT syncs it", got)
	}
}

// readTagged returns the tagged line itself, whatever it says: a row about a
// refusal has to read one.
func readTagged(t *testing.T, rd *bufio.Reader, tag string) string {
	t.Helper()
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, tag+" ") {
			return line
		}
	}
}

// uidsOfSelected asks the mailbox for its uids, in order.
func uidsOfSelected(t *testing.T, conn net.Conn, rd *bufio.Reader, tag string) []string {
	t.Helper()
	var out []string
	for _, line := range command(t, conn, rd, tag, "FETCH 1:* (UID)") {
		if i := strings.Index(line, "UID "); i >= 0 {
			out = append(out, strings.TrimRight(strings.Fields(line[i:])[1], ")"))
		}
	}
	return out
}

// A uid is stable for the life of a UIDVALIDITY: a client that caches by uid
// reads one message as another otherwise.
func TestVirtualUIDsSurviveASecondSelect(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "first", nil)
			saveInto(t, box, ui, "INBOX", 2, "second", nil)
		})
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d, want 2", got)
	}
	first := uidsOfSelected(t, conn, rd, "a3")

	// The same mailbox again: nothing changed, so nothing may be renumbered.
	if got := existsCount(t, conn, rd, "a4", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d on the second select, want 2", got)
	}
	second := uidsOfSelected(t, conn, rd, "a5")
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Errorf("the uids changed between selects: %v then %v", first, second)
	}

	// New mail takes the next number; the ones already here keep theirs.
	raw := "Subject: third\r\nFrom: a@test\r\n\r\nbody\r\n"
	fmt.Fprintf(conn, "a6 APPEND INBOX {%d+}\r\n%s\r\n", len(raw), raw)
	if line := readTagged(t, rd, "a6"); !strings.Contains(line, "OK") {
		t.Fatalf("APPEND answered %q", line)
	}
	if got := existsCount(t, conn, rd, "a7", "Virtual/All"); got != 3 {
		t.Fatalf("EXISTS = %d after new mail, want 3", got)
	}
	third := uidsOfSelected(t, conn, rd, "a8")
	if len(third) != 3 || strings.Join(third[:2], ",") != strings.Join(first, ",") {
		t.Errorf("new mail renumbered what was here: %v became %v", first, third)
	}
}

// Clients ask LIST "" "*", and the virtual mailbox has to be in that answer,
// not only under a pattern that names its namespace.
func TestVirtualMailboxIsListed(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "a message", nil)
		})
	var names []string
	for _, line := range command(t, conn, rd, "a2", `LIST "" "*"`) {
		if strings.HasPrefix(line, "* LIST") {
			names = append(names, line)
		}
	}
	found := false
	for _, n := range names {
		if strings.HasSuffix(n, `"Virtual/All"`) || strings.HasSuffix(n, " Virtual/All") {
			found = true
			// Clients read the attributes: \Noselect hides a mailbox that
			// SELECT would open, and the stand showed exactly that.
			if strings.Contains(n, `\Noselect`) {
				t.Errorf("Virtual/All is listed as \\Noselect: %s", n)
			}
		}
	}
	if !found {
		t.Errorf("LIST \"\" \"*\" did not name Virtual/All: %v", names)
	}
}
