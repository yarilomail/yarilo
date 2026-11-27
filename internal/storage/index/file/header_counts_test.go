package file

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The header's counts are what STATUS answers MESSAGES, UNSEEN and DELETED
// from: after any run of appends, flag changes and expunges, in this process
// and another, they equal a count over the records, in both (#2661).
func TestTheHeaderCountsAreTheRecordCounts(t *testing.T) {
	cases := []struct {
		name    string
		prefill int  // records before the run: past a chunk, the writer defers what changes the unloaded ones
		stamped bool // appends stamp the uid list, as a maildir save does
	}{
		{"plain", 0, false},
		{"maildir stamps", 0, true},
		{"past a chunk", mailindex.ChunkRecords + 50, true},
	}
	for _, tc := range cases {
		for _, seed := range []uint64{1, 2, 3} {
			home := testHome(t.TempDir(), testUser)
			info := &mailbox.UserInfo{Username: testUser, Home: home}
			a := New().OpenUser(info).(*userHandle)
			b := New().OpenUser(info).(*userHandle)
			t.Cleanup(func() { a.Close(); b.Close() }) //nolint:errcheck
			fa, err := a.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			appendOne := func(h *userHandle, flags []string) error {
				m := &mailbox.MessageMeta{Size: 10, Flags: flags}
				if !tc.stamped {
					return h.AllocateAndAppend(fa.ID, m)
				}
				return h.AllocateAndAppendStamped(fa.ID, m, func(uid uint32) (string, *mailbox.MaildirStamp, error) {
					return "", &mailbox.MaildirStamp{UIDListSize: 10 * uid}, nil
				})
			}
			for i := 0; i < tc.prefill; i++ {
				if err := appendOne(a, nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := b.OpenFolder("INBOX", 0); err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(seed, 7))
			for step := 0; step < 300; step++ {
				h := []*userHandle{a, b}[rng.IntN(2)]
				msgs, err := h.GetMessages(fa.ID, mailbox.SeqSet{})
				if err != nil {
					t.Fatal(err)
				}
				switch op := rng.IntN(4); {
				case op == 0 || len(msgs) == 0:
					var flags []string
					if rng.IntN(2) == 0 {
						flags = []string{`\Seen`}
					}
					if err := appendOne(h, flags); err != nil {
						t.Fatal(err)
					}
				case op == 1:
					m := msgs[rng.IntN(len(msgs))]
					if err := h.ExpungeMessage(fa.ID, m.UID); err != nil {
						t.Fatal(err)
					}
				default:
					m := msgs[rng.IntN(len(msgs))]
					var flags []string
					for _, f := range []string{`\Seen`, `\Deleted`, `\Flagged`} {
						if rng.IntN(2) == 0 {
							flags = append(flags, f)
						}
					}
					if err := h.UpdateFlags(fa.ID, m.UID, flags, nil); err != nil {
						t.Fatal(err)
					}
				}
				for name, h := range map[string]*userHandle{"a": a, "b": b} {
					f, err := h.OpenFolder("INBOX", fa.UIDValidity)
					if err != nil {
						t.Fatal(err)
					}
					msgs, err := h.GetMessages(f.ID, mailbox.SeqSet{})
					if err != nil {
						t.Fatal(err)
					}
					var unseen, deleted uint32
					for _, m := range msgs {
						if !slices.Contains(m.Flags, `\Seen`) {
							unseen++
						}
						if slices.Contains(m.Flags, `\Deleted`) {
							deleted++
						}
					}
					if f.Messages != uint32(len(msgs)) || f.Unseen != unseen || f.Deleted != deleted {
						t.Fatalf("%s, seed %d step %d, process %s: header messages %d unseen %d deleted %d, records %d, %d, %d",
							tc.name, seed, step, name, f.Messages, f.Unseen, f.Deleted, len(msgs), unseen, deleted)
					}
				}
			}
		}
	}
}
