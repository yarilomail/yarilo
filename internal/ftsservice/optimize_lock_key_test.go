package ftsservice

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type boxesIndex struct {
	stubUserIndex
	boxes      []fts.MailboxRef
	mu         sync.Mutex
	compacted  []string
	lockedWith []string // folder key held when each compaction ran
	current    *string
}

func (i *boxesIndex) Mailboxes() []fts.MailboxRef { return i.boxes }
func (i *boxesIndex) DocCount() (uint64, error)   { return 0, nil }
func (i *boxesIndex) OptimizeMailbox(m fts.MailboxRef) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.compacted = append(i.compacted, m.Name)
	held := ""
	if i.current != nil {
		held = *i.current
	}
	i.lockedWith = append(i.lockedWith, held)
	return nil
}

// Whole-user optimize must hold each mailbox's OWN lock, not a single
// user-keyed one. Every other FTS writer -- index jobs, rescan,
// auto-optimize -- keys on (user, folder); a compaction holding
// FTSKey(user, "") excluded none of them, so across processes it deleted
// shards another pod was reading or extending (#1176). Asserted on the key
// actually held while each compaction runs, which is the only thing that
// distinguishes the fix from a loop that merely visits every mailbox.
func TestOptimizeHoldsTheUsersIndexLock(t *testing.T) {
	dir := t.TempDir()
	chain, err := language.NewMultiChain(nil, nil, nil, 100, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	idx := &boxesIndex{boxes: []fts.MailboxRef{
		{GUID: "g1", Name: "INBOX", UIDValidity: 1},
		{GUID: "g2", Name: "Sent", UIDValidity: 1},
	}}
	var held string
	idx.current = &held

	svc, err := New(Options{
		Engine:  &fixedIndexEngine{idx: idx},
		Mailbox: maildir.New(),
		Index:   file.New(),
		ResolveUser: func(u string) (*mailbox.UserInfo, error) {
			home := filepath.Join(dir, u)
			return &mailbox.UserInfo{Username: u, Home: home, MailPath: home, Driver: "maildir"}, nil
		},
		Chain: chain,
		LockMailbox: func(_, folder string, fn func() error) error {
			held = folder
			defer func() { held = "" }()
			return fn()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() }) //nolint:errcheck

	if err := svc.Optimize("alice@test.com"); err != nil {
		t.Fatal(err)
	}

	if len(idx.compacted) == 0 {
		t.Fatal("nothing was compacted")
	}
	// One index, one lock: every writer of it takes FTSKey(user, "") now, and
	// a folder in the key would exclude nobody (#1986).
	for i, folder := range idx.lockedWith {
		if folder != "" {
			t.Errorf("compaction %d ran under the lock for folder %q, but the index is the user's", i, folder)
		}
	}
}

// fixedIndexEngine always returns the same index, so the test can inspect it.
type fixedIndexEngine struct{ idx fts.UserIndex }

func (fixedIndexEngine) Name() string   { return "fixed" }
func (fixedIndexEngine) Caps() fts.Caps { return fts.Caps{} }
func (e *fixedIndexEngine) OpenUser(context.Context, fts.UserRef) (fts.UserIndex, error) {
	return e.idx, nil
}
func (fixedIndexEngine) Close() error { return nil }

func (i *boxesIndex) DropFolder(fts.MailboxRef) error         { return nil }
func (i *boxesIndex) DropOrphanFolders([]string) (int, error) { return 0, nil }
