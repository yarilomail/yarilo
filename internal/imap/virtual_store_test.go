package imap_test

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

var (
	flagsRe  = regexp.MustCompile(`FLAGS \(([^)]*)\)`)
	modseqRe = regexp.MustCompile(`MODSEQ \((\d+)\)`)
)

// fetchLine is the one FETCH response a command answered with.
func fetchLine(t *testing.T, conn net.Conn, rd *bufio.Reader, tag, cmd string) string {
	t.Helper()
	for _, line := range command(t, conn, rd, tag, cmd) {
		if strings.Contains(line, " FETCH ") {
			return line
		}
	}
	t.Fatalf("%s answered no FETCH", cmd)
	return ""
}

func flagsOf(t *testing.T, line string) string {
	t.Helper()
	m := flagsRe.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no FLAGS in %q", line)
	}
	return m[1]
}

func modseqOf(t *testing.T, line string) uint64 {
	t.Helper()
	m := modseqRe.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no MODSEQ in %q", line)
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	return n
}

// STORE in the virtual mailbox changes the message itself: the copy in its
// folder carries the flag, as a client reading that folder sees it.
func TestVirtualStoreReachesTheCopy(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "one", nil)
		})
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1", got)
	}
	command(t, conn, rd, "a3", `STORE 1 +FLAGS (\Flagged)`)
	if got := existsCount(t, conn, rd, "a4", "INBOX"); got != 1 {
		t.Fatalf("INBOX EXISTS = %d, want 1", got)
	}
	if f := flagsOf(t, fetchLine(t, conn, rd, "a5", "FETCH 1 (FLAGS)")); !strings.Contains(f, `\Flagged`) {
		t.Errorf("INBOX holds FLAGS (%s) after STORE in the virtual mailbox, want \\Flagged", f)
	}
}

// A STORE delta is applied to the copy as a delta, and the reply is the set the
// copy ended with: another session set a flag there after this one synced.
func TestVirtualStoreKeepsWhatTheCopyGainedSince(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "one", nil)
		})
	existsCount(t, conn, rd, "a2", "Virtual/All")
	other, ord := loginTo(t, lastVirtualAddr)
	existsCount(t, other, ord, "b2", "INBOX")
	command(t, other, ord, "b3", `STORE 1 +FLAGS (\Answered)`)

	reply := fetchLine(t, conn, rd, "a3", `STORE 1 +FLAGS (\Flagged)`)
	if f := flagsOf(t, reply); !strings.Contains(f, `\Answered`) || !strings.Contains(f, `\Flagged`) {
		t.Errorf("STORE answered FLAGS (%s), want the copy's set with both flags", f)
	}
	if f := flagsOf(t, fetchLine(t, other, ord, "b4", "FETCH 1 (FLAGS)")); !strings.Contains(f, `\Answered`) || !strings.Contains(f, `\Flagged`) {
		t.Errorf("INBOX holds FLAGS (%s), want \\Answered kept and \\Flagged added", f)
	}
}

// A STORE moves the virtual record's own modseq: a CONDSTORE client that asks
// what changed since it last looked is told about the message.
func TestVirtualStoreMovesTheRecordModseq(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "one", nil)
			saveInto(t, box, ui, "INBOX", 2, "two", nil)
		})
	command(t, conn, rd, "a2", "ENABLE CONDSTORE")
	if got := existsCount(t, conn, rd, "a3", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d, want 2", got)
	}
	// The mailbox's highest modseq, not message 1's: sync numbered both.
	var before uint64
	for _, line := range command(t, conn, rd, "a4", "FETCH 1:* (MODSEQ)") {
		if strings.Contains(line, " FETCH ") {
			before = max(before, modseqOf(t, line))
		}
	}
	reply := fetchLine(t, conn, rd, "a5", `STORE 1 +FLAGS (\Seen)`)
	if after := modseqOf(t, reply); after <= before {
		t.Errorf("STORE answered MODSEQ %d, want more than %d", after, before)
	}
	var changed []string
	for _, line := range command(t, conn, rd, "a6", fmt.Sprintf("FETCH 1:* (FLAGS) (CHANGEDSINCE %d)", before)) {
		if strings.Contains(line, " FETCH ") {
			changed = append(changed, line)
		}
	}
	if len(changed) != 1 || !strings.HasPrefix(changed[0], "* 1 FETCH") {
		t.Errorf("CHANGEDSINCE %d answered %v, want only the message the STORE changed", before, changed)
	}
}

// EXPUNGE in the virtual mailbox removes the message from its folder, and the
// virtual mailbox loses it too.
func TestVirtualExpungeRemovesTheCopy(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "keep", nil)
			saveInto(t, box, ui, "INBOX", 2, "drop", nil)
		})
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d, want 2", got)
	}
	command(t, conn, rd, "a3", `STORE 2 +FLAGS.SILENT (\Deleted)`)
	var expunged []string
	for _, line := range command(t, conn, rd, "a4", "EXPUNGE") {
		if strings.HasSuffix(line, " EXPUNGE") {
			expunged = append(expunged, line)
		}
	}
	if len(expunged) != 1 || expunged[0] != "* 2 EXPUNGE" {
		t.Errorf("EXPUNGE answered %v, want * 2 EXPUNGE", expunged)
	}
	if got := existsCount(t, conn, rd, "a5", "INBOX"); got != 1 {
		t.Errorf("INBOX EXISTS = %d after EXPUNGE in the virtual mailbox, want 1", got)
	}
	if got := existsCount(t, conn, rd, "a6", "Virtual/All"); got != 1 {
		t.Errorf("Virtual/All EXISTS = %d, want 1", got)
	}
}

// A record whose copy another session already expunged goes on EXPUNGE: there
// is nothing left to remove but the record, and the command still succeeds.
func TestVirtualExpungeDropsARecordWhoseCopyIsGone(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "gone", []string{`\Deleted`})
		})
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1", got)
	}
	other, ord := loginTo(t, lastVirtualAddr)
	existsCount(t, other, ord, "b2", "INBOX")
	command(t, other, ord, "b3", "EXPUNGE")

	var expunged []string
	for _, line := range command(t, conn, rd, "a3", "EXPUNGE") {
		if strings.HasSuffix(line, " EXPUNGE") {
			expunged = append(expunged, line)
		}
	}
	if len(expunged) != 1 || expunged[0] != "* 1 EXPUNGE" {
		t.Errorf("EXPUNGE answered %v, want * 1 EXPUNGE for the record whose copy is gone", expunged)
	}
}

// EXPUNGE in a virtual mailbox retracts the copy from the index under the
// copy's folder and uid, naming the message: the record itself was never indexed.
func TestVirtualExpungeRetractsTheCopy(t *testing.T) {
	fake := &fakeFTS{}
	// Only the flagged message is kept, so virtual uid 1 is INBOX uid 2.
	conn, rd := virtualServerFTS(t, map[string]string{"Flagged": "INBOX\n  flagged\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "plain", nil)
			saveInto(t, box, ui, "INBOX", 2, "flagged", []string{`\Flagged`})
		}, fake)
	if got := existsCount(t, conn, rd, "a2", "Virtual/Flagged"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1", got)
	}
	command(t, conn, rd, "a3", `STORE 1 +FLAGS.SILENT (\Deleted)`)
	command(t, conn, rd, "a4", "EXPUNGE")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		uids, guids := append([]uint32(nil), fake.expunges...), append([][16]byte(nil), fake.expungedGUIDs...)
		fake.mu.Unlock()
		if len(uids) > 0 {
			if len(uids) != 1 || uids[0] != 2 || guids[0] == ([16]byte{}) {
				t.Errorf("retracted uids %v guids %x, want INBOX uid 2 with its GUID", uids, guids)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no retraction reached the index")
}

// Each script files what it saw into its own folder, when the cause is FLAG
// and the mailbox is the one it is bound to.
const (
	sieveOnInbox = `require ["imapsieve", "environment", "fileinto", "mailbox"];
if allof (environment :is "imap.cause" "FLAG", environment :is "imap.mailbox" "INBOX") { keep; fileinto :create "ByInbox"; }`
	sieveOnVirtual = `require ["imapsieve", "environment", "fileinto", "mailbox"];
if allof (environment :is "imap.cause" "FLAG", environment :is "imap.mailbox" "Virtual/All") { keep; fileinto :create "ByVirtual"; }`
)

func withImapSieve(t *testing.T) func(*imapserver.Options) {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{"oninbox": sieveOnInbox, "onvirtual": sieveOnVirtual} {
		if err := os.WriteFile(filepath.Join(dir, name+".sieve"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	md, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	eng := sieve.New(config.SieveConfig{
		Enabled: true, MaxRedirects: 32, MaxActions: 32, MaxScriptSize: 65536,
		DefaultName: "yarilo", ImapSieveEnabled: true, ImapSieveScriptDir: dir,
	}, nil, nil, nil)
	return func(o *imapserver.Options) { o.SieveEngine, o.MetadataDict = eng, md }
}

// A STORE in a virtual mailbox is a FLAG event in both mailboxes, as the
// reference has it: the copy's folder runs its script on the copy, and the
// virtual mailbox runs its own script on the same message.
func TestVirtualStoreRunsImapSieveInBothMailboxes(t *testing.T) {
	for _, tc := range []struct {
		name, bind, script, filedInto string
	}{
		{"the copy's folder", "INBOX", "oninbox", "ByInbox"},
		{"the virtual mailbox", "Virtual/All", "onvirtual", "ByVirtual"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, rd := virtualServerWith(t, map[string]string{"All": "INBOX\n"},
				func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
					saveInto(t, box, ui, "INBOX", 1, "the copy", nil)
				}, nil, withImapSieve(t))
			command(t, conn, rd, "a2", fmt.Sprintf(`SETMETADATA %q (/shared/imapsieve/script %q)`, tc.bind, tc.script))
			if got := existsCount(t, conn, rd, "a3", "Virtual/All"); got != 1 {
				t.Fatalf("EXISTS = %d, want 1", got)
			}
			command(t, conn, rd, "a4", `STORE 1 +FLAGS (\Flagged)`)
			if got := existsCount(t, conn, rd, "a5", tc.filedInto); got != 1 {
				t.Fatalf("%s holds %d messages, want the one the script on %s filed", tc.filedInto, got, tc.bind)
			}
			line := strings.Join(command(t, conn, rd, "a6", "FETCH 1 (BODY.PEEK[HEADER.FIELDS (SUBJECT)])"), "\n")
			if !strings.Contains(line, "the copy") {
				t.Errorf("the script on %s filed %q, want the copy", tc.bind, line)
			}
		})
	}
}
