package file

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Each write names the call that made it, and no mutation is left unnamed:
// there is no siteless wrapper to reach one through (#1827).
func TestAWriteIsCountedUnderItsOwnCaller(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "iris@example.com")
	ui := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "iris@example.com", Home: home,
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	at := func(site string) float64 { return counterVecValue(t, metricLockAcquired, "exclusive", site) }

	cases := []struct {
		site string
		run  func() error
	}{
		{lockSiteAppend, func() error {
			return ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10})
		}},
		{lockSiteWriteFlags, func() error {
			return ui.UpdateFlags(f.ID, 1, []string{`\Seen`}, nil)
		}},
		{lockSiteExpunge, func() error {
			return ui.ExpungeMessage(f.ID, 1)
		}},
		{lockSiteAllocateUid, func() error {
			_, aerr := ui.AllocateUID(f.ID)
			return aerr
		}},
		{lockSiteNextModseq, func() error {
			_, merr := ui.NextModSeq(f.ID)
			return merr
		}},
	}
	for _, tc := range cases {
		before := at(tc.site)
		other := at(lockSiteTestWrite)
		if rerr := tc.run(); rerr != nil {
			t.Fatalf("%s: %v", tc.site, rerr)
		}
		if got := at(tc.site) - before; got != 1 {
			t.Errorf("%s moved its own counter by %v, want 1", tc.site, got)
		}
		if got := at(lockSiteTestWrite) - other; got != 0 {
			t.Errorf("%s was also counted under another name %v times", tc.site, got)
		}
	}
}

// A transaction is counted as one acquisition under its own name, whatever it
// carries: that is the number a command costs.
func TestATransactionIsCountedOnceUnderItsOwnName(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "iris@example.com")
	ui := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "iris@example.com", Home: home,
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	for i := 1; i <= 10; i++ {
		if aerr := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i), Size: 10}); aerr != nil {
			t.Fatal(aerr)
		}
	}

	before := counterVecValue(t, metricLockAcquired, "exclusive", lockSiteTransaction)
	tx, terr := ui.Begin(f.ID)
	if terr != nil {
		t.Fatal(terr)
	}
	for i := 1; i <= 10; i++ {
		tx.Expunge(uint32(i))
	}
	if _, cerr := tx.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	if got := counterVecValue(t, metricLockAcquired, "exclusive", lockSiteTransaction) - before; got != 1 {
		t.Errorf("a transaction over ten records took %v acquisitions, want 1", got)
	}
}
