package acl

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
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

// gatedDict holds every registry iteration until released and counts them by path.
type gatedDict struct {
	dict.Dict
	release chan struct{}
	entered chan struct{} // receives each iteration as it starts, when set
	mu      sync.Mutex
	calls   map[string]int
}

func (g *gatedDict) Iterate(ctx context.Context, set *dict.OpSettings, path string, flags dict.IterFlag) (dict.Iterator, error) {
	g.mu.Lock()
	g.calls[path]++
	g.mu.Unlock()
	if g.entered != nil {
		g.entered <- struct{}{}
	}
	<-g.release
	return g.Dict.Iterate(ctx, set, path, flags)
}

// Sessions missing the same path at once share one scan: after a pod start
// every LIST missed anyone/ together and filled the dict pool (#2381).
func TestConcurrentMissesShareOneScan(t *testing.T) {
	d := regDict(t)
	clockFor(t)
	grant(t, d, "alice", mailbox.Identifier{Type: mailbox.IDAnyone})
	const sessions = 20
	joined := make(chan struct{}, sessions)
	joinedScan = func() { joined <- struct{}{} }
	t.Cleanup(func() { joinedScan = func() {} })
	g := &gatedDict{Dict: d, release: make(chan struct{}), calls: map[string]int{}}

	path := fwdPrefix + "anyone/"
	answers := make([][]string, sessions)
	errs := make([]error, sessions)
	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			answers[i], errs[i] = owners.cachedPath(context.Background(), g, path)
		}(i)
	}
	for n := 0; n < sessions-1; n++ {
		select {
		case <-joined:
		case <-time.After(2 * time.Second):
			t.Fatalf("%d of %d misses waited for the scan in flight; the rest scanned on their own", n, sessions-1)
		}
	}
	close(g.release)
	wg.Wait()
	if g.calls[path] != 1 {
		t.Errorf("%d sessions scanned %s %d times, want once", sessions, path, g.calls[path])
	}
	for i := range answers {
		if errs[i] != nil || len(answers[i]) != 1 || answers[i][0] != "alice" {
			t.Fatalf("session %d answered %v, %v; want [alice]", i, answers[i], errs[i])
		}
	}
}

// The session that started a scan leaving mid-way does not answer for the
// others: the scan runs on, once, and every waiter gets its owners.
func TestALeavingSessionDoesNotFailTheScan(t *testing.T) {
	d := regDict(t)
	clockFor(t)
	grant(t, d, "alice", mailbox.Identifier{Type: mailbox.IDAnyone})
	const waiters = 5
	joined := make(chan struct{}, waiters)
	joinedScan = func() { joined <- struct{}{} }
	t.Cleanup(func() { joinedScan = func() {} })
	g := &gatedDict{Dict: d, release: make(chan struct{}), entered: make(chan struct{}, 1), calls: map[string]int{}}
	path := fwdPrefix + "anyone/"

	leaderCtx, leave := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := owners.cachedPath(leaderCtx, g, path)
		leaderErr <- err
	}()
	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the leading session's scan never started")
	}
	answers := make([][]string, waiters)
	errs := make([]error, waiters)
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			answers[i], errs[i] = owners.cachedPath(context.Background(), g, path)
		}(i)
	}
	for n := 0; n < waiters; n++ {
		select {
		case <-joined:
		case <-time.After(2 * time.Second):
			t.Fatalf("%d of %d waiters joined the scan", n, waiters)
		}
	}
	leave()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Errorf("the leaving session answered %v, want its own cancellation", err)
	}
	close(g.release)
	wg.Wait()
	for i := range answers {
		if errs[i] != nil || len(answers[i]) != 1 || answers[i][0] != "alice" {
			t.Errorf("waiter %d answered %v, %v after the leader left; want [alice]", i, answers[i], errs[i])
		}
	}
	if g.calls[path] != 1 {
		t.Errorf("%s iterated %d times, want once", path, g.calls[path])
	}
}
