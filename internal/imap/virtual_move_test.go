package imap_test

import (
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Only the flagged message is kept, so virtual uid 1 is INBOX uid 2: a COPYUID
// or an expunge that names the copy's uid instead of the record's shows here.
const flaggedConfig = "INBOX\n  flagged\n"

func seedFlagged(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
	saveInto(t, box, ui, "INBOX", 1, "plain", nil)
	saveInto(t, box, ui, "INBOX", 2, "the flagged one", []string{`\Flagged`})
	box.Create("Archive") //nolint:errcheck
	if _, err := ui.OpenFolder("Archive", 0); err != nil {
		t.Fatal(err)
	}
}

func subjectOf(t *testing.T, lines []string) string {
	t.Helper()
	for _, l := range lines {
		if strings.HasPrefix(l, "Subject: ") {
			return strings.TrimPrefix(l, "Subject: ")
		}
	}
	return ""
}

// COPY out of a virtual mailbox copies the real message, and COPYUID names the
// virtual uid it was asked about (virtual-storage.c:978).
func TestCopyOutOfAVirtualMailboxCopiesTheRealMessage(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"Flagged": flaggedConfig}, seedFlagged)
	if got := existsCount(t, conn, rd, "a2", "Virtual/Flagged"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1", got)
	}
	lines := tagged(t, conn, rd, "a3", "UID COPY 1 Archive")
	if answer := last(lines); !strings.Contains(answer, "OK [COPYUID ") || !strings.Contains(answer, " 1 1]") {
		t.Errorf("COPY answered %q, want COPYUID naming virtual uid 1", answer)
	}
	if got := existsCount(t, conn, rd, "a4", "Archive"); got != 1 {
		t.Fatalf("Archive holds %d, want the copy", got)
	}
	if s := subjectOf(t, command(t, conn, rd, "a5", "FETCH 1 (BODY.PEEK[HEADER.FIELDS (SUBJECT)])")); s != "the flagged one" {
		t.Errorf("Archive holds %q, want the real message's bytes", s)
	}
	if got := existsCount(t, conn, rd, "a6", "INBOX"); got != 2 {
		t.Errorf("INBOX holds %d after a COPY, want both still", got)
	}
}

// MOVE out of a virtual mailbox moves the real message: it arrives in the
// destination and leaves its own folder (virtual-mail.c:541-548).
func TestMoveOutOfAVirtualMailboxMovesTheRealMessage(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"Flagged": flaggedConfig}, seedFlagged)
	existsCount(t, conn, rd, "a2", "Virtual/Flagged")
	lines := tagged(t, conn, rd, "a3", "UID MOVE 1 Archive")
	if answer := last(lines); !strings.HasPrefix(answer, "a3 OK") {
		t.Fatalf("MOVE answered %v", lines)
	}
	copyuid := linesWith(lines, "COPYUID")
	if len(copyuid) != 1 || !strings.Contains(copyuid[0], " 1 1]") {
		t.Errorf("MOVE answered COPYUID %v, want virtual uid 1 as the source", copyuid)
	}
	if got := linesWith(lines, " EXPUNGE"); len(got) != 1 || got[0] != "* 1 EXPUNGE" {
		t.Errorf("MOVE expunged %v, want * 1 EXPUNGE", got)
	}
	if got := existsCount(t, conn, rd, "a4", "INBOX"); got != 1 {
		t.Errorf("INBOX holds %d after the MOVE, want the flagged message gone", got)
	}
	if got := existsCount(t, conn, rd, "a5", "Archive"); got != 1 {
		t.Errorf("Archive holds %d, want the moved message", got)
	}
	if s := subjectOf(t, command(t, conn, rd, "a6", "FETCH 1 (BODY.PEEK[HEADER.FIELDS (SUBJECT)])")); s != "the flagged one" {
		t.Errorf("Archive holds %q, want the moved message", s)
	}
}

// The copy's expunge is its folder's: the index retracts INBOX uid 2 with its
// GUID, not the virtual record.
func TestMoveOutOfAVirtualMailboxRetractsTheCopy(t *testing.T) {
	fake := &fakeFTS{}
	conn, rd := virtualServerFTS(t, map[string]string{"Flagged": flaggedConfig}, seedFlagged, fake)
	existsCount(t, conn, rd, "a2", "Virtual/Flagged")
	command(t, conn, rd, "a3", "UID MOVE 1 Archive")
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

// MOVE to a folder the virtual mailbox also draws from leaves the message in
// the set: it comes back as a new record at the next sync, EXISTS unchanged.
func TestMoveWithinTheSetKeepsTheCount(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\nArchive\n"}, seedFlagged)
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d, want 2", got)
	}
	before := uidsOfSelected(t, conn, rd, "a3")
	command(t, conn, rd, "a4", "UID MOVE "+before[1]+" Archive")
	if got := existsCount(t, conn, rd, "a5", "Virtual/All"); got != 2 {
		t.Errorf("EXISTS = %d after moving within the set, want 2", got)
	}
	after := uidsOfSelected(t, conn, rd, "a6")
	if len(after) != 2 || after[0] != before[0] || after[1] == before[1] {
		t.Errorf("uids %v became %v, want the moved message under a new uid and the other kept", before, after)
	}
}
