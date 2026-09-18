package acl

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// clockFor gives the cache a hand-wound clock and resets everything the rows
// in this file share.
func clockFor(t *testing.T) *time.Time {
	t.Helper()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	InvalidateAll()
	ResetScans()
	owners.mu.Lock()
	owners.now = func() time.Time { return now }
	owners.mu.Unlock()
	t.Cleanup(func() {
		owners.mu.Lock()
		owners.now = time.Now
		owners.mu.Unlock()
		InvalidateAll()
		ResetScans()
	})
	return &now
}

func grant(t *testing.T, d dict.Dict, owner string, ids ...mailbox.Identifier) {
	t.Helper()
	entries := make([]ListEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, regEntry(id, "lr", false))
	}
	if err := NewRegistry(d, owner).SyncFromList(entries, true); err != nil {
		t.Fatal(err)
	}
}

func lookup(t *testing.T, d dict.Dict, user string, groups ...string) []string {
	t.Helper()
	got, err := OwnersFor(context.Background(), d, user, groups)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	return got
}

// Two LIST commands in one session read the registry once: the answer is a
// discovery answer, and it is held for the interval (#1875).
func TestTwoListsReadTheRegistryOnce(t *testing.T) {
	d := regDict(t)
	clockFor(t)
	grant(t, d, "alice", mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"})

	ResetScans()
	first := lookup(t, d, "bob")
	after := Scans()
	second := lookup(t, d, "bob")

	if len(first) != 1 || first[0] != "alice" || len(second) != 1 || second[0] != "alice" {
		t.Fatalf("the two lists answered %v and %v, want [alice] twice", first, second)
	}
	// user/bob/ and anyone/: two paths, each read once.
	if after != 2 {
		t.Errorf("the first list read %d registry paths, want 2", after)
	}
	if Scans() != after {
		t.Errorf("the second list read the registry again (%d scans, was %d)", Scans(), after)
	}
}

// A second login of the same user reads nothing either: the interval lives in
// the process, not in the session, and a stand does four thousand logins an
// hour (#1875).
func TestASecondLoginReadsNothing(t *testing.T) {
	d := regDict(t)
	clockFor(t)
	grant(t, d, "alice", mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"})

	ResetScans()
	lookup(t, d, "bob")
	first := Scans()
	// A new session is a new call with no state of its own.
	lookup(t, d, "bob")
	if Scans() != first {
		t.Errorf("the second login scanned again: %d scans, was %d", Scans(), first)
	}
}

// anyone/ and group/<g>/ are the same rows for everyone who reads them, so one
// process reads each once however many users ask.
func TestSharedPathsAreReadOncePerProcess(t *testing.T) {
	d := regDict(t)
	clockFor(t)
	grant(t, d, "alice",
		mailbox.Identifier{Type: mailbox.IDGroup, Name: "staff"},
		mailbox.Identifier{Type: mailbox.IDAnyone})

	ResetScans()
	one := lookup(t, d, "bob", "staff")
	two := lookup(t, d, "carol", "staff")
	if len(one) != 1 || len(two) != 1 {
		t.Fatalf("the two users discovered %v and %v, want alice for both", one, two)
	}
	// bob: user/bob/, anyone/, group/staff/ = 3. carol adds only user/carol/.
	if got := Scans(); got != 4 {
		t.Errorf("two users of one group cost %d scans, want 4 (three paths plus the second user's own)", got)
	}
}

// The owner's own SETACL shows in their next LIST; another user's grant waits
// out the interval, as in the reference.
func TestOwnSetaclIsSeenAtOnce(t *testing.T) {
	d := regDict(t)
	now := clockFor(t)
	grant(t, d, "bob", mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"})
	if got := lookup(t, d, "bob"); len(got) != 1 || got[0] != "bob" {
		t.Fatalf("bob does not see his own grant: %v", got)
	}

	// Somebody else grants bob something: bob keeps the answer he has.
	grant(t, d, "alice", mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"})
	if got := lookup(t, d, "bob"); len(got) != 1 {
		t.Errorf("another user's grant arrived before the interval: %v", got)
	}

	// Bob's own write invalidates what bob was told.
	grant(t, d, "bob", mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"})
	got := lookup(t, d, "bob")
	if len(got) != 2 {
		t.Errorf("after bob's own SETACL he sees %v, want both owners", got)
	}

	// And the interval brings it to everyone else.
	*now = now.Add(OwnerScanTTL + time.Minute)
	if got := lookup(t, d, "carol"); len(got) != 0 {
		t.Errorf("carol was granted nothing and sees %v", got)
	}
}

// The interval ends: the next read goes to the dict again.
func TestTheIntervalExpires(t *testing.T) {
	d := regDict(t)
	now := clockFor(t)
	grant(t, d, "alice", mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"})
	lookup(t, d, "bob")
	before := Scans()

	*now = now.Add(OwnerScanTTL - time.Minute)
	lookup(t, d, "bob")
	if Scans() != before {
		t.Errorf("a read inside the interval scanned: %d, was %d", Scans(), before)
	}
	*now = now.Add(2 * time.Minute)
	lookup(t, d, "bob")
	if Scans() <= before {
		t.Errorf("a read after the interval did not scan: %d, was %d", Scans(), before)
	}
}
