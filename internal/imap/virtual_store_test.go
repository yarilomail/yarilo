package imap_test

import (
	"bufio"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"

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
