package file

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// tornSize is the size every record of the stress folder carries for its uid,
// so a meta whose fields came from two records cannot pass for one.
func tornSize(uid uint32) uint32 { return uid*10 + 7 }

// Sessions of one process read every record while another session and another
// process append, expunge and change flags: each meta must be one record's
// fields, as a COPY then writes them into a new one (#2660).
func TestAReadNeverMixesTwoRecords(t *testing.T) {
	root := t.TempDir()
	home := testHome(root, testUser)
	open := func(be *Backend) mailbox.UserIndex {
		return be.OpenUser(&mailbox.UserInfo{Username: testUser, Home: home})
	}
	local, remote := New(), New()
	w := open(local)
	f, err := w.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	add := func(h mailbox.UserIndex) error {
		uid, err := h.AllocateUID(f.ID)
		if err != nil {
			return err
		}
		return h.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: tornSize(uid), MapUID: uid, SaveDate: 1788000000})
	}
	for i := 0; i < 600; i++ {
		if err := add(w); err != nil {
			t.Fatal(err)
		}
	}
	other := open(remote)
	if _, err := other.OpenFolder("INBOX", 0); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var torn, reads atomic.Int64
	var firstTorn atomic.Value
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		reader := open(local)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				msgs, err := reader.(*userHandle).GetMessagesUnlocked(f.ID, mailbox.SeqSet{})
				if err != nil {
					continue
				}
				reads.Add(1)
				for _, m := range msgs {
					if m.MapUID != m.UID || m.Size != tornSize(m.UID) {
						if torn.Add(1) == 1 {
							firstTorn.Store(fmt.Sprintf("uid %d read map uid %d and size %d", m.UID, m.MapUID, m.Size))
						}
					}
				}
			}
		}()
	}
	writers := []mailbox.UserIndex{w, other}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 1500; i++ {
		h := writers[i%2]
		msgs, err := h.GetMessages(f.ID, mailbox.SeqSet{})
		if err != nil || len(msgs) == 0 {
			t.Fatalf("writer read: %d messages, %v", len(msgs), err)
		}
		pick := msgs[rng.IntN(len(msgs))].UID
		switch rng.IntN(3) {
		case 0:
			err = add(h)
		case 1:
			err = h.ExpungeMessage(f.ID, pick)
		default:
			err = h.UpdateFlags(f.ID, pick, []string{`\Seen`}, nil)
		}
		if err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("%d whole reads by the sessions", reads.Load())
	if n := torn.Load(); n > 0 {
		t.Fatalf("%d metas mixed two records; the first: %v", n, firstTorn.Load())
	}
}
