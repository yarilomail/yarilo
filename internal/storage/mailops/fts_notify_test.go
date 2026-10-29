package mailops

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/fts"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// notifyRecorder records what the index was told, by folder and uid.
type notifyRecorder struct {
	dropRecorder
	mu   sync.Mutex
	told []string
}

func (n *notifyRecorder) Index(_ string, m fts.MailboxRef, uid uint32, _ int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.told = append(n.told, fmt.Sprintf("index %s %d", m.Name, uid))
	return nil
}

func (n *notifyRecorder) Expunge(_ string, m fts.MailboxRef, uid uint32, _ [16]byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.told = append(n.told, fmt.Sprintf("expunge %s %d", m.Name, uid))
	return nil
}

func (n *notifyRecorder) wait(t *testing.T, want int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n.mu.Lock()
		got := slices.Clone(n.told)
		n.mu.Unlock()
		if len(got) >= want || time.Now().After(deadline) {
			time.Sleep(50 * time.Millisecond)
			n.mu.Lock()
			got = slices.Clone(n.told)
			n.mu.Unlock()
			slices.Sort(got)
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The index hears of a delivery when autoindex is on, of an expunge and of a
// retraction always, and of nothing in a folder with no identity to key by.
func TestTheIndexHearsWhatChanged(t *testing.T) {
	for name, c := range map[string]struct {
		autoindex bool
		want      []string
	}{
		"autoindex on":  {true, []string{"expunge INBOX 2", "expunge INBOX 3", "index INBOX 1"}},
		"autoindex off": {false, []string{"expunge INBOX 2", "expunge INBOX 3"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &notifyRecorder{}
			o := New(Config{Protocol: "test", FTS: rec, FTSAutoindex: c.autoindex}, nil, &mailbox.UserInfo{Username: "u@example.com"})
			f := &mailbox.Folder{Name: "INBOX", GUID: [16]byte{1}}
			o.Committed(f, locks.EventDelivered, 1, 0, [16]byte{})
			o.Committed(f, locks.EventExpunged, 2, 0, [16]byte{})
			o.Retracted(f, 3, [16]byte{})
			o.Retracted(&mailbox.Folder{Name: "NoGUID"}, 4, [16]byte{})
			if got := rec.wait(t, len(c.want)); !slices.Equal(got, c.want) {
				t.Errorf("the index was told %v, want %v", got, c.want)
			}
		})
	}
}

// A folder that cannot be indexed is warned about once, not once per message.
func TestAFolderWithoutGUIDIsWarnedOnce(t *testing.T) {
	log := logged(t)
	for range 3 {
		WarnFolderWithoutGUID("test", "once@example.com", "NoGUID")
	}
	if n := strings.Count(log.String(), "NoGUID"); n != 1 {
		t.Errorf("warned %d times, want once", n)
	}
}
