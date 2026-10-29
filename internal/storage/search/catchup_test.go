package search

import (
	"errors"
	"sync"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/v2/pkg/fts"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// steppingIndex reports its checkpoint from a script, one value per poll, the
// last repeated.
type steppingIndex struct {
	fakeIndex
	mu    sync.Mutex
	steps []uint32
}

func (s *steppingIndex) Status(string, fts.MailboxRef) (uint32, uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.steps[0]
	if len(s.steps) > 1 {
		s.steps = s.steps[1:]
	}
	return v, 0, nil
}

// A SEARCH behind the index waits while the index moves and is answered by it;
// a stalled index, warm or never started, gives up as still indexing.
func TestASearchWaitsForAnIndexThatMoves(t *testing.T) {
	c := &imaplib.SearchCriteria{Body: []string{"needle"}}
	_, msgs, _ := folderOf(t)
	ref := RefOf(&mailbox.Folder{Name: "INBOX", GUID: [16]byte{1}})
	for name, tc := range map[string]struct {
		steps          []uint32
		timeout, grace time.Duration
		answered       bool
	}{
		"climbing to the last uid":              {[]uint32{1, 2, 3}, 5 * time.Second, 0, true},
		"stuck one short":                       {[]uint32{1, 2}, 5 * time.Second, 0, false},
		"never started, grace past the timeout": {[]uint32{0}, 600 * time.Millisecond, time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			idx := &steppingIndex{fakeIndex: fakeIndex{definite: []uint32{1}}, steps: tc.steps}
			o := Options{Client: idx, Chain: chain(t), Enabled: true, AddMissing: "priority", Timeout: tc.timeout, FirstIndexGrace: tc.grace}
			p, err := o.Plan("u", ref, c, msgs)
			if tc.answered {
				if err != nil || p == nil || !p.Covered[1] {
					t.Errorf("plan %v, %v; want the index's answer", p, err)
				}
				return
			}
			if !errors.Is(err, ErrStillIndexing) {
				t.Errorf("plan answered %v, want still indexing", err)
			}
		})
	}
}

// A strict SEARCH reads the index's definite hits as well as its maybes.
func TestAStrictPlanVerifiesDefiniteHits(t *testing.T) {
	_, msgs, _ := folderOf(t)
	ref := RefOf(&mailbox.Folder{Name: "INBOX", GUID: [16]byte{1}})
	o := Options{Client: &fakeIndex{indexed: 3, definite: []uint32{1}, maybe: []uint32{3}}, Chain: chain(t), Enabled: true, Strict: true}
	p, err := o.Plan("u", ref, &imaplib.SearchCriteria{Body: []string{"needle"}}, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Verify[1] || !p.Verify[3] {
		t.Errorf("verify %v, want both hits read", p.Verify)
	}
}
