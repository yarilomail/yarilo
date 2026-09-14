package mailboxbase_test

import (
	"path/filepath"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// reopen builds a second box over the same account, as a reconnect produces.
func reopen(t *testing.T, home string) mailbox.Box {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: home}
	box := maildir.New().OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	idx := fileidx.New().OpenUser(info)
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck
	return mailboxbase.Open(box, idx)
}

// The case the cache is for: a client that logs in per cycle, which a
// session-scoped cache never gates at all (#1248).
func TestAFreshBoxReusesTheProcessCache(t *testing.T) {
	b, inbox := gateSetup(t)
	settle(t, inbox, time.Unix(1700000000, 0))
	if _, err := b.Folder("INBOX", 0); err != nil {
		t.Fatal(err)
	}
	scanned, skipped := syncCount(t, "scanned"), syncCount(t, "skipped")

	home := filepath.Dir(inbox)
	for i := 0; i < 5; i++ {
		if _, err := reopen(t, home).Folder("INBOX", 0); err != nil {
			t.Fatal(err)
		}
	}

	if got := syncCount(t, "scanned") - scanned; got != 0 {
		t.Errorf("%v walks over 5 fresh boxes on an unchanged folder, want 0", got)
	}
	if got := syncCount(t, "skipped") - skipped; got != 5 {
		t.Errorf("skips = %v, want 5", got)
	}
}

// The other direction: a file that appeared between two logins is still picked
// up, because the token and not the cache entry decides.
func TestAFreshBoxStillSeesOutOfBandDelivery(t *testing.T) {
	settled := time.Unix(1700000000, 0)
	b, inbox := gateSetup(t)
	settle(t, inbox, settled)
	if _, err := b.Folder("INBOX", 0); err != nil {
		t.Fatal(err)
	}

	deliverOutOfBand(t, inbox, "1700000001.M1P1_1.host:2,S", settled.Add(time.Minute))

	next := reopen(t, filepath.Dir(inbox))
	if _, err := next.Folder("INBOX", 0); err != nil {
		t.Fatal(err)
	}
	if got := messageCount(t, next); got != 1 {
		t.Errorf("messages = %d, want 1 -- a file delivered between logins was missed", got)
	}
}

// Two accounts must not answer for each other even when their folder names
// match: the second one's token was never proven, so it must cost a walk.
func TestASecondAccountIsNotAnsweredByTheFirstsToken(t *testing.T) {
	b, inbox := gateSetup(t)
	settle(t, inbox, time.Unix(1700000000, 0))
	if _, err := b.Folder("INBOX", 0); err != nil {
		t.Fatal(err)
	}
	scanned := syncCount(t, "scanned")

	other := &mailbox.UserInfo{Username: "v@x.com", Home: filepath.Join(t.TempDir(), "x.com", "v")}
	obox := maildir.New().OpenUser(other)
	defer obox.Close() //nolint:errcheck
	if err := obox.Init(); err != nil {
		t.Fatal(err)
	}
	oidx := fileidx.New().OpenUser(other)
	defer oidx.Close() //nolint:errcheck
	if _, err := mailboxbase.Open(obox, oidx).Folder("INBOX", 1); err != nil {
		t.Fatal(err)
	}

	if got := syncCount(t, "scanned") - scanned; got != 1 {
		t.Errorf("scans for a second account = %v, want 1 (its own token was never proven)", got)
	}
}
