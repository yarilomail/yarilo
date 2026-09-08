package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A shared hold is not an exclusive one: a write that took it for ours would
// run with no exclusive lock at all (#1741).
func TestASharedHoldDoesNotSatisfyAWrite(t *testing.T) {
	lk := newHoldsClient(t, holdsTestSocket(t))
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: "dave@example.com", Home: home}
	mb := maildir.New(maildir.WithLocker(lk)).OpenUser(user)
	if err := mb.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	name, _, _, err := mb.Save("INBOX", strings.NewReader("body"), 1, 4, nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	name, err = mailbox.Driver(mb).(mailbox.UIDNamer).AssignUID("INBOX", name, 1)
	if err != nil {
		t.Fatalf("assign uid: %v", err)
	}

	key := locks.MailboxKey(user.Username, "INBOX")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shared, err := lk.LockShared(locks.WithSite(ctx, "read"), key, "test/0/dave@example.com/sess1", 30*time.Second)
	if err != nil {
		t.Fatalf("shared lock: %v", err)
	}
	defer func() { _ = lk.Unlock(ctx, shared.ID) }()

	refusedBefore := counterValue(t, "yarilo_locks_hold_upgrade_refused_total", "site", "write-flags")
	acquiresBefore := acquireCount(t, "mbox")
	_, rerr := mailbox.Driver(mb).(mailbox.FlagWriter).WriteFlags("INBOX", name, []string{`\Seen`}, nil)
	if !errors.Is(rerr, locks.ErrHoldNotExclusive) {
		t.Fatalf("the write under a shared hold answered %v, want ErrHoldNotExclusive", rerr)
	}
	if !strings.Contains(rerr.Error(), "write-flags") {
		t.Errorf("the refusal is %q and does not name the site that wanted the lock", rerr)
	}
	if got := counterValue(t, "yarilo_locks_hold_upgrade_refused_total", "site", "write-flags") - refusedBefore; got != 1 {
		t.Errorf("the refusal was counted %v times at its site, want 1: production cannot see it", got)
	}
	if got := acquireCount(t, "mbox") - acquiresBefore; got != 0 {
		t.Errorf("the refused write made %v acquisitions; it must answer without asking the service", got)
	}
}

// The counter names the mode held: a read inside a locked write is carried by
// an exclusive hold, and counting it shared invents holds nobody took (#1741).
func TestTheReentrantCounterNamesTheModeHeld(t *testing.T) {
	lk := newHoldsClient(t, holdsTestSocket(t))
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: "erin@example.com", Home: home}
	idx := file.New(file.WithLocker(lk)).OpenUser(user)
	t.Cleanup(func() { _ = idx.Close() })
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}

	key := locks.MailboxKey(user.Username, "INBOX")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outer, err := lk.Lock(locks.WithSite(ctx, "write"), key, "test/0/erin@example.com/sess1", 30*time.Second)
	if err != nil {
		t.Fatalf("outer lock: %v", err)
	}
	defer func() { _ = lk.Unlock(ctx, outer.ID) }()

	beforeX := reentrantCount(t, "exclusive")
	beforeS := reentrantCount(t, "shared")
	if _, err := idx.GUIDBackfillNeeded(folder.ID); err != nil {
		t.Fatalf("read under the write hold: %v", err)
	}
	if got := reentrantCount(t, "exclusive") - beforeX; got < 1 {
		t.Errorf("the read under an exclusive hold counted %v exclusive re-entries, want at least 1", got)
	}
	if got := reentrantCount(t, "shared") - beforeS; got != 0 {
		t.Errorf("the read counted %v shared re-entries; no shared lock was ever taken", got)
	}
}

// An outer exclusive hold still carries a batch: POP3 QUIT and multi-message
// EXPUNGE take one lock and then call per-message storage methods.
func TestAnOuterExclusiveHoldStillCarriesABatch(t *testing.T) {
	lk := newHoldsClient(t, holdsTestSocket(t))
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: "frank@example.com", Home: home}
	mb := maildir.New(maildir.WithLocker(lk)).OpenUser(user)
	idx := file.New(file.WithLocker(lk)).OpenUser(user)
	t.Cleanup(func() { _ = idx.Close() })
	if err := mb.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	names := make([]string, 0, 3)
	uids := make([]uint32, 0, 3)
	for i := 0; i < 3; i++ {
		uid, aerr := idx.AllocateUID(folder.ID)
		if aerr != nil {
			t.Fatal(aerr)
		}
		name, _, _, serr := mb.Save("INBOX", strings.NewReader("body"), uid, 4, nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		if aerr := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid}); aerr != nil {
			t.Fatal(aerr)
		}
		names = append(names, name)
		uids = append(uids, uid)
	}

	key := locks.MailboxKey(user.Username, "INBOX")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outer, err := lk.Lock(locks.WithSite(ctx, "write"), key, "test/0/frank@example.com/sess1", 30*time.Second)
	if err != nil {
		t.Fatalf("outer lock: %v", err)
	}
	defer func() { _ = lk.Unlock(ctx, outer.ID) }()

	reentrantBefore := reentrantCount(t, "exclusive")
	acquiresBefore := acquireCount(t, "mbox")
	for i, name := range names {
		if rerr := mb.Remove("INBOX", name); rerr != nil {
			t.Fatalf("batch remove %d: %v", i, rerr)
		}
		if eerr := idx.ExpungeMessage(folder.ID, uids[i]); eerr != nil {
			t.Fatalf("batch expunge %d: %v", i, eerr)
		}
	}
	if got := reentrantCount(t, "exclusive") - reentrantBefore; got != 3 {
		t.Errorf("the batch re-entered the outer hold %v times, want 3 -- one per expunge", got)
	}
	if got := acquireCount(t, "mbox") - acquiresBefore; got != 0 {
		t.Errorf("the batch made %v acquisitions of its own; the outer hold carries it", got)
	}
}

// acquireCount is how many blocking acquisitions were made on one resource
// class, counted at the client: an attempt that blocks is counted too.
func acquireCount(t *testing.T, class string) float64 {
	t.Helper()
	return sumMetric(t, "yarilo_locks_acquire_wait_seconds", "resource", class)
}

// counterValue reads one counter, summed over every series carrying label=value.
func counterValue(t *testing.T, name, label, value string) float64 {
	t.Helper()
	return sumMetric(t, name, label, value)
}

func sumMetric(t *testing.T, name, label, value string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() != label || l.GetValue() != value {
					continue
				}
				if h := m.GetHistogram(); h != nil {
					total += float64(h.GetSampleCount())
				}
				if c := m.GetCounter(); c != nil {
					total += c.GetValue()
				}
			}
		}
	}
	return total
}

// reentrantCount reads fileindex_lock_reentrant_total for one mode, over every
// site.
func reentrantCount(t *testing.T, mode string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, f := range families {
		if f.GetName() != "fileindex_lock_reentrant_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "mode" && l.GetValue() == mode {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}
