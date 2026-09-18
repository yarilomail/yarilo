package acl

import (
	"context"
	"errors"
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

// A grant drops the answer of the principal it names, not of the owner who
// granted: on the backend that took the SETACL the recipient sees the share in
// their next LIST, and a user the grant does not name waits out the interval.
func TestAGrantIsSeenByItsRecipientAtOnce(t *testing.T) {
	d := regDict(t)
	now := clockFor(t)

	// Both read once, so both have an answer held.
	if got := lookup(t, d, "bob"); len(got) != 0 {
		t.Fatalf("bob starts with %v, want nothing", got)
	}
	if got := lookup(t, d, "carol"); len(got) != 0 {
		t.Fatalf("carol starts with %v, want nothing", got)
	}

	grant(t, d, "alice", mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"})

	if got := lookup(t, d, "bob"); len(got) != 1 || got[0] != "alice" {
		t.Errorf("the recipient sees %v after the grant, want [alice]", got)
	}
	if got := lookup(t, d, "carol"); len(got) != 0 {
		t.Errorf("a user the grant does not name sees %v before the interval", got)
	}

	// And carol, who was never granted anything, still sees nothing after it.
	*now = now.Add(OwnerScanTTL + time.Minute)
	if got := lookup(t, d, "carol"); len(got) != 0 {
		t.Errorf("carol was granted nothing and sees %v", got)
	}
}

// A grant to a group drops the group's answer, which is shared: every member
// on this backend sees it, and that is one entry, not one per member.
func TestAGroupGrantIsSeenByTheGroup(t *testing.T) {
	d := regDict(t)
	clockFor(t)
	lookup(t, d, "bob", "staff")
	lookup(t, d, "carol", "staff")

	grant(t, d, "alice", mailbox.Identifier{Type: mailbox.IDGroup, Name: "staff"})

	for _, who := range []string{"bob", "carol"} {
		if got := lookup(t, d, who, "staff"); len(got) != 1 || got[0] != "alice" {
			t.Errorf("%s sees %v after a grant to their group, want [alice]", who, got)
		}
	}
}

// A dict that fails is asked once per interval, not once per LIST: the
// interval counts the attempt, as the reference counts its check.
func TestAFailedScanIsNotRetriedPerList(t *testing.T) {
	d := regDict(t)
	now := clockFor(t)
	grant(t, d, "alice", mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"})
	lookup(t, d, "bob") // an answer is held

	broken := &failingDict{Dict: d}
	*now = now.Add(OwnerScanTTL + time.Minute)
	ResetScans()

	first, err := OwnersFor(context.Background(), broken, "bob", nil)
	if err != nil {
		t.Fatalf("a failed scan with an answer in hand returned an error: %v", err)
	}
	if len(first) != 1 || first[0] != "alice" {
		t.Errorf("the failed scan answered %v, want the last good answer [alice]", first)
	}
	after := Scans()

	if _, err := OwnersFor(context.Background(), broken, "bob", nil); err != nil {
		t.Fatal(err)
	}
	if Scans() != after {
		t.Errorf("the next LIST retried the failed scan: %d scans, was %d", Scans(), after)
	}
}

// failingDict answers every iteration with an error, as a dict service that is
// down does.
type failingDict struct {
	dict.Dict
}

func (f *failingDict) Iterate(context.Context, *dict.OpSettings, string, dict.IterFlag) (dict.Iterator, error) {
	return nil, errBroken
}

var errBroken = errors.New("acl test: the dict is down")
