package imap_test

import (
	"bufio"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailboxmetrics"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// searchUIDs is what UID SEARCH answers, in order.
func searchUIDs(t *testing.T, conn net.Conn, rd *bufio.Reader, tag, criteria string) []string {
	t.Helper()
	var out []string
	for _, line := range command(t, conn, rd, tag, "UID SEARCH "+criteria) {
		if strings.HasPrefix(line, "* SEARCH") {
			out = append(out, strings.Fields(strings.TrimPrefix(line, "* SEARCH"))...)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeFTS) asked() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.lookupIns...)
}

// A SEARCH over the set is one question to the index, not one per folder: the
// set is what the user searches, and N folders must not cost N round trips.
func TestVirtualSearchAsksOnceOverEveryBackingFolder(t *testing.T) {
	fake := &fakeFTS{setHits: map[string][]uint32{"INBOX": {1}, "Archive": {1}}}
	conn, rd := virtualServerFTS(t, map[string]string{"All": "INBOX\nArchive\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "alpha needle", nil)
			saveInto(t, box, ui, "INBOX", 2, "beta", nil)
			saveInto(t, box, ui, "Archive", 1, "gamma needle", nil)
		}, fake)
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 3 {
		t.Fatalf("EXISTS = %d, want 3", got)
	}
	if got := searchUIDs(t, conn, rd, "a3", "TEXT needle"); len(got) != 2 {
		t.Errorf("SEARCH answered %v, want the two messages the index names", got)
	}
	asked := fake.asked()
	if len(asked) != 1 {
		t.Fatalf("the index was asked %d times (%v), want once over the set", len(asked), asked)
	}
	sort.Strings(asked[0])
	if strings.Join(asked[0], ",") != "Archive,INBOX" {
		t.Errorf("the one question named %v, want both backing folders", asked[0])
	}
}

// A folder the configuration takes out is not part of the set, so the index is
// not asked about it and what it holds does not answer.
func TestVirtualSearchLeavesOutANegativeFolder(t *testing.T) {
	fake := &fakeFTS{setHits: map[string][]uint32{"INBOX": {1}, "Archive": {1}}}
	conn, rd := virtualServerFTS(t, map[string]string{"All": "*\n-Archive\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "needle here", nil)
			saveInto(t, box, ui, "Archive", 1, "needle there", nil)
		}, fake)
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1: Archive is taken out", got)
	}
	if got := searchUIDs(t, conn, rd, "a3", "TEXT needle"); len(got) != 1 {
		t.Errorf("SEARCH answered %v, want only the INBOX message", got)
	}
	for _, names := range fake.asked() {
		for _, n := range names {
			if n == "Archive" {
				t.Errorf("the index was asked about Archive, which the set leaves out: %v", names)
			}
		}
	}
}

// A backing folder the rule keeps nothing of has no message to answer for, so
// SEARCH neither waits for its index nor asks about it.
func TestVirtualSearchSkipsAFolderWithNothingKept(t *testing.T) {
	fake := &fakeFTS{
		setHits: map[string][]uint32{"INBOX": {1}},
		behind:  map[string]bool{"Archive": true},
	}
	conn, rd := virtualServerFTS(t, map[string]string{"All": "INBOX\nArchive\n  unseen\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "needle here", nil)
			saveInto(t, box, ui, "Archive", 1, "needle there", []string{`\Seen`})
		}, fake)
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1: the rule keeps nothing of Archive", got)
	}
	if got := searchUIDs(t, conn, rd, "a3", "TEXT needle"); len(got) != 1 {
		t.Errorf("SEARCH answered %v, want the INBOX message", got)
	}
	fake.mu.Lock()
	jobs := append([]string(nil), fake.jobFolders...)
	fake.mu.Unlock()
	for _, j := range jobs {
		if j == "Archive" {
			t.Errorf("SEARCH caught up Archive, which holds nothing here: %v", jobs)
		}
	}
	for _, names := range fake.asked() {
		for _, n := range names {
			if n == "Archive" {
				t.Errorf("the index was asked about Archive: %v", names)
			}
		}
	}
}

// A copy in two folders is two messages here, so a hit on it is two uids: the
// index answers per GUID, and each copy in the set has to come back.
func TestVirtualSearchAnswersEachCopy(t *testing.T) {
	fake := &fakeFTS{setHits: map[string][]uint32{"INBOX": {1}, "Archive": {1}}}
	conn, rd := virtualServerFTS(t, map[string]string{"All": "INBOX\nArchive\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			guid := saveInto(t, box, ui, "INBOX", 1, "needle", nil)
			saveCopy(t, box, ui, "Archive", 1, "needle", nil, guid)
		}, fake)
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d, want 2", got)
	}
	if got := searchUIDs(t, conn, rd, "a3", "TEXT needle"); len(got) != 2 {
		t.Errorf("SEARCH answered %v, want both copies", got)
	}
}

// A backing folder the index has not reached is read, not answered from what
// the index has: a silent miss there looks like a message that is not there.
func TestVirtualSearchReadsAFolderTheIndexIsBehindOn(t *testing.T) {
	fake := &fakeFTS{
		setHits: map[string][]uint32{"INBOX": {1}},
		behind:  map[string]bool{"Archive": true},
	}
	conn, rd := virtualServerFTS(t, map[string]string{"All": "INBOX\nArchive\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "needle here", nil)
			saveInto(t, box, ui, "Archive", 1, "needle there", nil)
			saveInto(t, box, ui, "Archive", 2, "nothing", nil)
		}, fake)
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 3 {
		t.Fatalf("EXISTS = %d, want 3", got)
	}
	if got := searchUIDs(t, conn, rd, "a3", "SUBJECT needle"); len(got) != 2 {
		t.Errorf("SEARCH answered %v, want the INBOX hit and the Archive message read in full", got)
	}
	for _, names := range fake.asked() {
		for _, n := range names {
			if n == "Archive" {
				t.Errorf("the index was asked about Archive before it caught up: %v", names)
			}
		}
	}
}

// The virtual mailbox holds no message of its own, so no index job may name
// it: one would index every backing message a second time under its name.
func TestVirtualSearchQueuesNoIndexJobOnItself(t *testing.T) {
	fake := &fakeFTS{setHits: map[string][]uint32{"INBOX": {1}}}
	conn, rd := virtualServerFTS(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "needle", nil)
		}, fake)
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1", got)
	}
	if got := searchUIDs(t, conn, rd, "a3", "TEXT needle"); len(got) != 1 {
		t.Errorf("SEARCH answered %v, want the one hit", got)
	}
	fake.mu.Lock()
	jobs := append([]string(nil), fake.jobFolders...)
	fake.mu.Unlock()
	if len(jobs) == 0 {
		t.Fatal("no index job at all: the backing folder was never caught up, so the row reads nothing")
	}
	// The session names a folder without its namespace prefix, so the check
	// is by what may be named, not by the prefix it lacks.
	for _, j := range jobs {
		if j != "INBOX" {
			t.Errorf("an index job named %q, not a backing folder: %v", j, jobs)
		}
	}
}

// A text rule is answered by the index when the mailbox syncs: reading every
// backing body on each SELECT is what the index exists to avoid.
func TestVirtualTextRuleAtSyncOpensNoBody(t *testing.T) {
	fake := &fakeFTS{setHits: map[string][]uint32{"INBOX": {1}}}
	conn, rd := virtualServerFTS(t, map[string]string{"Needles": "INBOX\n  subject needle\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "needle one", nil)
			saveInto(t, box, ui, "INBOX", 2, "other", nil)
			saveInto(t, box, ui, "INBOX", 3, "another", nil)
		}, fake)
	before := mailboxmetrics.MessageOpens("maildir")
	if got := existsCount(t, conn, rd, "a2", "Virtual/Needles"); got != 1 {
		t.Fatalf("EXISTS = %d, want the 1 message the rule keeps", got)
	}
	if opened := mailboxmetrics.MessageOpens("maildir") - before; opened != 0 {
		t.Errorf("the sync opened %v bodies, want none: the index answers the rule", opened)
	}
	if asked := fake.asked(); len(asked) != 1 {
		t.Errorf("the index was asked %d times at sync, want once: %v", len(asked), asked)
	}
}

// Without the index a text criterion reads the copy: a virtual record holds no
// bytes, and a match decided without them answers every message.
func TestVirtualSearchWithoutTheIndexReadsTheCopy(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "needle here", nil)
			saveInto(t, box, ui, "INBOX", 2, "nothing", nil)
		})
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d, want 2", got)
	}
	if got := searchUIDs(t, conn, rd, "a3", "SUBJECT needle"); len(got) != 1 {
		t.Errorf("SEARCH answered %v, want the one message whose subject matches", got)
	}
}

// FETCH of a virtual message answers what FETCH of its copy answers, for
// every item read from the bytes: the record itself holds none.
func TestVirtualFetchReadsTheCopy(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "needle here", nil)
		})
	items := []string{"ENVELOPE", "BODYSTRUCTURE", "BODY.PEEK[HEADER.FIELDS (SUBJECT)]",
		"BODY.PEEK[]", "BINARY.PEEK[1]", "BINARY.SIZE[1]"}
	answers := func(mailboxName, tag string) []string {
		if got := existsCount(t, conn, rd, tag+"s", mailboxName); got != 1 {
			t.Fatalf("EXISTS in %s = %d, want 1", mailboxName, got)
		}
		var out []string
		for i, item := range items {
			out = append(out, strings.Join(command(t, conn, rd, fmt.Sprintf("%s%d", tag, i), "FETCH 1 ("+item+")"), "\n"))
		}
		return out
	}
	real, virt := answers("INBOX", "r"), answers("Virtual/All", "v")
	if !strings.Contains(real[0], "needle here") {
		t.Fatalf("INBOX ENVELOPE answered %q: the row compares nothing", real[0])
	}
	for i, item := range items {
		if virt[i] != real[i] {
			t.Errorf("FETCH %s: virtual answered %q, its copy %q", item, virt[i], real[i])
		}
	}
}
