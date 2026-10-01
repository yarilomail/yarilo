package mailboxbase

import (
	"log/slog"

	"github.com/yarilomail/yarilo/internal/msgcache"
	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// boxTx remembers the modseq each record was read at, so a change applies to
// the record as read and is skipped once it moved (#1805).
type boxTx struct {
	b        *Box
	folderID uint64
	tx       mailbox.IndexTx
	seen     map[uint32]uint64
	expected map[uint32]bool
}

// Begin opens one command's changes to a folder; nothing is held until Commit.
func (b *Box) Begin(folderID uint64) (mailbox.BoxTx, error) {
	tx, err := b.index.Begin(folderID)
	if err != nil {
		return nil, err
	}
	return &boxTx{b: b, folderID: folderID, tx: tx, seen: map[uint32]uint64{}, expected: map[uint32]bool{}}, nil
}

// Messages reads under the folder's lock, as a read that decides a write must
// (#1249), and holds nothing after it.
func (t *boxTx) Messages(set mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	msgs, err := t.b.index.GetMessages(t.folderID, set)
	for _, m := range msgs {
		t.seen[m.UID] = m.ModSeq
	}
	return msgs, err
}

// expect ties the changes on uid to the modseq it was read at, once.
func (t *boxTx) expect(uid uint32) {
	if ms, ok := t.seen[uid]; ok && !t.expected[uid] {
		t.tx.Expect(uid, ms)
		t.expected[uid] = true
	}
}

func (t *boxTx) UpdateFlags(uid uint32, upd mailbox.FlagsUpdate) {
	t.expect(uid)
	t.tx.UpdateFlags(uid, upd)
}

func (t *boxTx) Expunge(uid uint32) {
	t.expect(uid)
	t.tx.Expunge(uid)
}

func (t *boxTx) Append(m *mailbox.MessageMeta)     { t.tx.Append(m) }
func (t *boxTx) Commit() (mailbox.TxResult, error) { return t.tx.Commit() }
func (t *boxTx) Rollback()                         { t.tx.Rollback() }

// unlockedReader is an index whose files prove their own freshness, so a read
// that only answers a client skips the cross-process lock (#1249).
type unlockedReader interface {
	VanishedUnlocked(folderID uint64, sinceModSeq uint64) ([]uint32, error)
	KeywordsUnlocked(folderID uint64) ([]string, error)
}

func (b *Box) Vanished(folderID uint64, sinceModSeq uint64) ([]uint32, error) {
	if u, ok := b.index.(unlockedReader); ok {
		return u.VanishedUnlocked(folderID, sinceModSeq)
	}
	return b.index.Vanished(folderID, sinceModSeq)
}

func (b *Box) Keywords(folderID uint64) ([]string, error) {
	if u, ok := b.index.(unlockedReader); ok {
		return u.KeywordsUnlocked(folderID)
	}
	return b.index.Keywords(folderID)
}

// CreateFolder writes the index with the folder, so the first open is not taken
// for a lost one; an index not written now is written by that open (#1608).
func (b *Box) CreateFolder(name string, uidValidity uint32) {
	fc, ok := b.index.(mailbox.FolderCreator)
	if !ok {
		return
	}
	if _, err := fc.CreateFolder(name, uidValidity); err != nil {
		slog.Warn("mailbox: folder index not created with the folder", "user", b.store.Username(), "folder", name, "err", err)
	}
}

func (b *Box) DeleteFolder(name string) error { return b.index.DeleteFolder(name) }
func (b *Box) RenameFolder(oldName, newName string) error {
	return b.index.RenameFolder(oldName, newName)
}

func (b *Box) Metadata(folderID uint64) (mailbox.FolderMetadata, error) {
	vsize, msgs, err := b.index.FolderVSize(folderID)
	return mailbox.FolderMetadata{VSize: vsize, Messages: msgs}, err
}

// RecordExists reports whether the folder still holds uid; an unreadable index
// answers yes, so a real fault is not taken for an expunge.
func (b *Box) RecordExists(folderID uint64, uid uint32) bool {
	msgs, err := b.index.GetMessages(folderID, mailbox.SeqSet{{From: uid, To: uid}})
	if err != nil {
		return true
	}
	for _, m := range msgs {
		if m.UID == uid {
			return true
		}
	}
	return false
}

func (b *Box) RebuildFolder(f *mailbox.Folder) (int, error) {
	st, err := idxrebuild.RebuildFolder(b, b.index, f)
	return st.UIDsAssigned + st.UIDsPreserved, err
}

func (b *Box) BackfillGUIDs(f *mailbox.Folder, name string) error {
	return idxrebuild.BackfillGUIDs(b, b.index, f, name)
}

// EnvelopeCache opens the folder's cache; a nil handle answers every call as a
// miss, so none served is not an error to the protocol.
func (b *Box) EnvelopeCache(folderID uint64, opts mailbox.EnvelopeCacheOptions) mailbox.EnvelopeCache {
	return msgcache.Open(b.index, folderID, msgcache.Options{
		Locker: opts.Locker, User: opts.User, SessionID: opts.SessionID, Folder: opts.Folder,
		TraceID: opts.TraceID, DeferWrites: opts.DeferWrites, Shared: opts.Shared,
	})
}
