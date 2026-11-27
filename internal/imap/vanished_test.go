package imap

import (
	"reflect"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// historyBox answers Vanished from a fixed history, reaching or not.
type historyBox struct {
	mailbox.Box
	uids    []uint32
	reaches bool
}

func (b historyBox) Vanished(uint64, uint64) ([]uint32, bool, error) { return b.uids, b.reaches, nil }

// VANISHED keeps to the client's uids below UIDNEXT, and past the history names
// every uid in range the folder lacks, not the few the logs still hold (#2664).
func TestVanishedSinceAnswersFromTheFolderPastTheHistory(t *testing.T) {
	held := []*mailbox.MessageMeta{{UID: 2}, {UID: 5}, {UID: 6}}
	set := func(rs ...[2]uint32) imaplib.UIDSet {
		var s imaplib.UIDSet
		for _, r := range rs {
			s.AddRange(imaplib.UID(r[0]), imaplib.UID(r[1]))
		}
		return s
	}
	tests := []struct {
		name    string
		history []uint32
		reaches bool
		within  imaplib.UIDSet
		want    imaplib.UIDSet
	}{
		{"history, all uids", []uint32{3, 4, 9}, true, nil, set([2]uint32{3, 4})},
		{"history, the named uids", []uint32{3, 4}, true, set([2]uint32{4, 6}), set([2]uint32{4, 4})},
		{"past the history, all uids", []uint32{4}, false, nil, set([2]uint32{1, 1}, [2]uint32{3, 4}, [2]uint32{7, 7})},
		{"past the history, the named uids", []uint32{4}, false, set([2]uint32{2, 4}), set([2]uint32{3, 4})},
		{"past the history, to *", nil, false, imaplib.UIDSet{{Start: 6, Stop: 0}}, set([2]uint32{7, 7})},
		{"past the history, nothing gone", nil, false, set([2]uint32{5, 6}), nil},
		{"past the history, ranges out of order and overlapping", nil, false, imaplib.UIDSet{{Start: 6, Stop: 7}, {Start: 1, Stop: 3}, {Start: 2, Stop: 4}}, set([2]uint32{1, 1}, [2]uint32{3, 4}, [2]uint32{7, 7})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vanishedSince(historyBox{uids: tc.history, reaches: tc.reaches}, 1, 10, tc.within, 8, held)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("VANISHED %v, want %v", got, tc.want)
			}
		})
	}
}

// A folder that has used nearly all of the uid space and holds three messages
// costs what three messages cost: the answer past the history walks the
// messages, not the uids. A walk over the uids would take minutes here.
func TestVanishedPastTheHistoryCostsTheMessagesNotTheUIDSpace(t *testing.T) {
	const next = mailbox.MaxUID + 1
	held := []*mailbox.MessageMeta{{UID: next - 10}, {UID: 4}, {UID: 500_000}}
	done := make(chan imaplib.UIDSet, 1)
	go func() {
		got, err := vanishedSince(historyBox{reaches: false}, 1, 10, nil, next, held)
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	select {
	case got := <-done:
		want := imaplib.UIDSet{{Start: 1, Stop: 3}, {Start: 5, Stop: 499_999}, {Start: 500_001, Stop: next - 11}, {Start: next - 9, Stop: next - 1}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("VANISHED %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer for three messages walked the uid space")
	}
}
