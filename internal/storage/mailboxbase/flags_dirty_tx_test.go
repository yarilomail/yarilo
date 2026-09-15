package mailboxbase_test

import (
	"fmt"
	"strings"
	"testing"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// refusingStore writes flags like maildir but refuses the uids named, so the
// command has both kinds of outcome in one batch.
type refusingStore struct {
	mailbox.UserMailbox
	refuse map[uint32]struct{}
}

func (r *refusingStore) WriteFlagsMulti(folder string, writes []mailbox.FlagWrite) []mailbox.FlagWriteResult {
	inner, _ := mailbox.Driver(r.UserMailbox).(mailbox.FlagWriterMulti)
	out := make([]mailbox.FlagWriteResult, 0, len(writes))
	var pass []mailbox.FlagWrite
	for _, w := range writes {
		if _, no := r.refuse[w.UID]; no {
			out = append(out, mailbox.FlagWriteResult{UID: w.UID, Err: fmt.Errorf("storage refused")})
			continue
		}
		pass = append(pass, w)
	}
	if len(pass) > 0 && inner != nil {
		out = append(out, inner.WriteFlagsMulti(folder, pass)...)
	}
	return out
}

func (r *refusingStore) Driver() mailbox.UserMailbox { return r }

// A command's dirty marks are one transaction, not one acquisition per message:
// that was 1633 of 3318 index acquisitions in a 90-second window (#1809).
func TestDirtyMarksTakeTheIndexOncePerCommand(t *testing.T) {
	const messages = 40

	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: home, Driver: "maildir"}
	lk := &countingLocker{held: map[string]locks.HoldMode{}}
	store := maildir.New().OpenUser(info)
	idx := fileidx.New(fileidx.WithLocker(lk)).OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mailboxbase.SetTestSyncTokens(8))

	box := mailboxbase.Open(store, idx)
	f, err := box.Folder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	writes := make([]mailbox.FlagWrite, 0, messages)
	for i := 0; i < messages; i++ {
		uid, aerr := idx.AllocateUID(f.ID)
		if aerr != nil {
			t.Fatal(aerr)
		}
		body := fmt.Sprintf("From: a@b\r\nSubject: m%d\r\n\r\nbody\r\n", i)
		saved, vsize, guid, serr := store.Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
		if rerr := box.RecordDelivered(f, "INBOX", saved, m); rerr != nil {
			t.Fatal(rerr)
		}
		writes = append(writes, mailbox.FlagWrite{UID: uid, Filename: saved, Flags: []string{`\Seen`}})
	}

	// Two of the forty do not reach storage.
	refused := map[uint32]struct{}{writes[3].UID: {}, writes[17].UID: {}}
	refusing := &refusingStore{UserMailbox: store, refuse: refused}

	before := journalHolds(t)
	results := mailboxbase.FlagsWritten(idx, refusing, f.ID, "INBOX", writes)
	took := journalHolds(t) - before
	t.Logf("locks taken recording %d flag writes: %d", messages, took)
	if len(results) != messages {
		t.Fatalf("recorded %d results, want %d", len(results), messages)
	}
	// The driver's own acquisition plus the index once. Never one per message.
	if took > 2 {
		t.Errorf("recording %d flag writes took %d locks, want the command's own few", messages, took)
	}

	msgs, gerr := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	var dirty int
	for _, m := range msgs {
		if m.FlagsDirty {
			dirty++
			if _, want := refused[m.UID]; !want {
				t.Errorf("uid %d is marked dirty and its write landed", m.UID)
			}
		}
	}
	if dirty != len(refused) {
		t.Errorf("%d records are marked dirty, want %d", dirty, len(refused))
	}
}
