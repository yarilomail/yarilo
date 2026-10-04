package imap

import (
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// markingBox answers RecordExists from a set the test controls and counts the
// folders marked corrupt.
type markingBox struct {
	mailbox.Box
	present map[uint32]bool
	marked  int
}

func (b *markingBox) RecordExists(_ uint64, uid uint32) bool { return b.present[uid] }

func (b *markingBox) MarkCorruptOnFetchErr(string, error) bool { b.marked++; return true }

// A read that lost to another session's expunge marks nothing: sdbox calls a
// vanished file corruption, and under load that is what an expunge is (#1690).
func TestAReadThatLostToAnExpungeMarksNothing(t *testing.T) {
	idx := &markingBox{present: map[uint32]bool{}} // uid 7 already expunged
	s := &session{userInfo: &mailbox.UserInfo{Username: "u@example.com"}}
	err := errors.Join(errors.New("open: no such file"), mailbox.ErrCorruptStorage)

	s.flagCorruptOnRead(idx, 1, "INBOX", "u.7", 7, err)
	if idx.marked != 0 {
		t.Errorf("the folder was marked corrupt %d times: a message another session "+
			"expunged is not damage", idx.marked)
	}
}

// A record whose file went away with the record still in the index is damage,
// and still marks: that is the case the heal exists for.
func TestARecordWithNoFileStillMarks(t *testing.T) {
	idx := &markingBox{present: map[uint32]bool{7: true}}
	s := &session{userInfo: &mailbox.UserInfo{Username: "u@example.com"}}
	err := errors.Join(errors.New("open: no such file"), mailbox.ErrCorruptStorage)

	s.flagCorruptOnRead(idx, 1, "INBOX", "u.7", 7, err)
	if idx.marked != 1 {
		t.Errorf("the folder was marked %d times, want 1: a dangling record is what the "+
			"reactive heal is for", idx.marked)
	}
}
