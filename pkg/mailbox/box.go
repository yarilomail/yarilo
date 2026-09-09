package mailbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// Box is one account's mail, both halves together: every rule needing the store
// and the index lives here, so a consumer asks for a message (#1715).
type Box struct {
	store  UserMailbox
	index  UserIndex
	locker locks.Locker
	owner  string
}

// Open pairs the two halves of one account.
func Open(store UserMailbox, index UserIndex, opts ...BoxOption) *Box {
	b := &Box{store: store, index: index}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Store is the half that holds bodies. Consumers still reaching for it are the
// ones this arc has not moved yet.
func (b *Box) Store() UserMailbox { return b.store }

// Index is the half that holds records, on the same terms.
func (b *Box) Index() UserIndex { return b.index }

// Username is whose mail this is.
func (b *Box) Username() string { return b.store.Username() }

// Folder opens one folder's index.
func (b *Box) Folder(name string, uidValidity uint32) (*Folder, error) {
	return b.index.OpenFolder(name, uidValidity)
}

// RecordDelivered records a saved body: the name reaches storage first, so a
// failed name leaves no record behind (#1745).
func (b *Box) RecordDelivered(f *Folder, folder, saved string, m *MessageMeta) error {
	if err := NameSaved(b.store, folder, saved, m); err != nil {
		return fmt.Errorf("mailbox/deliver: name %q: %w", saved, err)
	}
	if err := b.index.AppendMessage(f.ID, m); err != nil {
		return fmt.Errorf("mailbox/deliver: append uid %d: %w", m.UID, err)
	}
	return nil
}

// FillSizes gives the records that carry no size the one their storage holds,
// so a sum over the folder is taken on mail and not on zeros (#1728).
func (b *Box) FillSizes(f *Folder) (int, error) {
	return FillSizelessRecords(b.index, b.store, f)
}

// WithLocker gives the box the cross-process lock client, so a rule needing one
// hold over many messages has one to take.
func WithLocker(l locks.Locker, owner string) BoxOption {
	return func(b *Box) { b.locker, b.owner = l, owner }
}

// BoxOption tunes a box at Open time.
type BoxOption func(*Box)

// ExpungeMarked removes messages under one hold: taking the key per message
// lets another writer in between two removals. One failure is not the batch's.
func (b *Box) ExpungeMarked(f *Folder, folder string, msgs []*MessageMeta) (removed []uint32, failed int) {
	if b.locker == nil {
		return b.expungeEach(f, folder, msgs)
	}
	key := locks.MailboxKey(b.store.Username(), folder)
	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "pop3-batch"), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, b.locker, key, b.owner, 30*time.Second)
	if err != nil {
		slog.Error("mailbox/expunge: the batch could not take the folder; falling back to one hold per message",
			"user", b.store.Username(), "folder", folder, "err", err)
		return b.expungeEach(f, folder, msgs)
	}
	defer func() { _ = b.locker.Unlock(ctx, lk.ID) }()
	return b.expungeEach(f, folder, msgs)
}

// expungeEach reads the name, removes the record, then the body: a stop between
// the last two leaves a file for the next rebuild, never a record with no file (#1690).
func (b *Box) expungeEach(f *Folder, folder string, msgs []*MessageMeta) (removed []uint32, failed int) {
	for _, m := range msgs {
		// The name before the record: a driver named by uid reads it out of
		// the record this loop is about to remove (#1712).
		name, nameErr := MessagePath(b.store, folder, m)
		if err := b.index.ExpungeMessage(f.ID, m.UID); err != nil {
			slog.Error("mailbox/expunge: record", "user", b.store.Username(),
				"folder", folder, "uid", m.UID, "err", err)
			failed++
			continue
		}
		removed = append(removed, m.UID)
		if testAfterRecordExpunged != nil {
			testAfterRecordExpunged()
		}
		if nameErr != nil || name == "" {
			slog.Warn("mailbox/expunge: the record named no file; its body, if any, stays",
				"user", b.store.Username(), "folder", folder, "uid", m.UID, "err", nameErr)
			continue
		}
		if err := b.store.Remove(folder, name); err != nil {
			slog.Error("mailbox/expunge: body", "user", b.store.Username(),
				"folder", folder, "uid", m.UID, "file", name, "err", err)
		}
	}
	return removed, failed
}

// testAfterRecordExpunged runs between the record and the body. Test seam: the
// crash window the order is chosen for.
var testAfterRecordExpunged func()

// SetTestAfterRecordExpunged arms that seam and returns a function disarming it.
func SetTestAfterRecordExpunged(fn func()) func() {
	testAfterRecordExpunged = fn
	return func() { testAfterRecordExpunged = nil }
}

// Close releases both halves. A half that will not close is logged and the
// other still closes: a session ending must not leave one handle open.
func (b *Box) Close() {
	if err := b.store.Close(); err != nil {
		slog.Warn("mailbox: closing the store", "user", b.store.Username(), "err", err)
	}
	if b.index == nil {
		return
	}
	if err := b.index.Close(); err != nil {
		slog.Warn("mailbox: closing the index", "user", b.store.Username(), "err", err)
	}
}
