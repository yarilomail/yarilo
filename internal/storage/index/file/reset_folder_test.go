package file

import (
	"slices"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A rebuild's records go in as given: holes skipped, \Deleted kept, and a
// virtual folder's record keeps the copy it names, in every process.
func TestAResetFolderKeepsWhatEachRecordCarries(t *testing.T) {
	dial := lockstest.Server(t)
	home := t.TempDir()
	a, b := process(t, home, dial()), process(t, home, dial())
	f := inbox(t, a)
	must(t, a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}))
	setter := a.(interface {
		SetVirtualHeader(uint64, mailbox.VirtualHeader) error
	})
	must(t, setter.SetVirtualHeader(f.ID, mailbox.VirtualHeader{}))

	_, err := a.ResetFolder(f.ID, []*mailbox.MessageMeta{
		nil,
		{UID: 0, Size: 10},
		{UID: 1, Size: 10, Flags: []string{`\Deleted`}},
		{UID: 2, Size: 10, VirtualBacking: 3, VirtualRealUID: 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	fb := inbox(t, b)
	msgs := allOf(t, b, fb.ID)
	if len(msgs) != 2 {
		t.Fatalf("%d records after the reset, want 2", len(msgs))
	}
	if !slices.Contains(msgs[0].Flags, `\Deleted`) {
		t.Errorf("uid 1 flags %v, want \\Deleted", msgs[0].Flags)
	}
	if msgs[1].VirtualBacking != 3 || msgs[1].VirtualRealUID != 9 {
		t.Errorf("uid 2 names backing %d uid %d, want 3 and 9", msgs[1].VirtualBacking, msgs[1].VirtualRealUID)
	}
}
