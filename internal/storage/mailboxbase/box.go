package mailboxbase

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Box is the shared base every driver's mailbox embeds: the rules that need the
// store and the index together live here, once (#1715).
type Box struct {
	store mailbox.UserMailbox
	index mailbox.UserIndex
	// mode is what an open owes the folder: settle, read, or only add (#1778).
	mode openMode
}

// openMode is what a box's Folder does besides opening the index.
type openMode uint8

const (
	// openSession settles the folder; the other two do not.
	openSession openMode = iota
	// openReadOnly writes nothing at all.
	openReadOnly
	// openSaveOnly is a delivery: the save writes its own row, so the walk
	// buys nothing and costs a hold (#1706).
	openSaveOnly
)

// Open pairs the two halves of one account.
func Open(store mailbox.UserMailbox, index mailbox.UserIndex, opts ...BoxOption) *Box {
	b := &Box{store: store, index: index}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Store is the half that holds bodies. Consumers still reaching for it are the
// ones this arc has not moved yet.
func (b *Box) Store() mailbox.UserMailbox { return b.store }

// Index is the half that holds records, on the same terms.
func (b *Box) Index() mailbox.UserIndex { return b.index }

// Username is whose mail this is.
func (b *Box) Username() string { return b.store.Username() }

// Folder opens one folder's index, settling what the store holds first. Only a
// session open settles; the other modes read the folder as it is.
func (b *Box) Folder(name string, uidValidity uint32) (*mailbox.Folder, error) {
	f, err := b.index.OpenFolder(name, uidValidity)
	if err != nil || b.mode != openSession {
		return f, err
	}
	if !b.settle(name, f) {
		return f, nil
	}
	// The record set moved, so the handle in hand is stale: UIDNEXT and
	// HIGHESTMODSEQ are what the caller reports.
	refreshed, rerr := b.index.OpenFolder(name, f.UIDValidity)
	if rerr != nil {
		// The handle in hand describes a folder that no longer exists; handing
		// it back would have the caller report a stale UIDNEXT as its own.
		return nil, fmt.Errorf("mailbox/open: %s reopen %q after settling: %w",
			b.store.Username(), name, rerr)
	}
	return refreshed, nil
}

// RecordDelivered records a saved body: the name reaches storage first, so a
// failed name leaves no record behind (#1745).
func (b *Box) RecordDelivered(f *mailbox.Folder, folder, saved string, m *mailbox.MessageMeta) error {
	if err := NameSaved(b.store, folder, saved, m); err != nil {
		return fmt.Errorf("mailbox/deliver: name %q: %w", saved, err)
	}
	if err := b.index.AppendMessage(f.ID, m); err != nil {
		return fmt.Errorf("mailbox/deliver: append uid %d: %w", m.UID, err)
	}
	return nil
}

// FillSizeless gives the records that carry no size the one their storage holds,
// so a sum over the folder is taken on mail and not on zeros (#1728).
func (b *Box) FillSizeless(f *mailbox.Folder) (int, error) {
	return FillSizelessRecords(b.index, b.store, f)
}

// Readable reports whether a record resolves to a body at all: one that does
// not is reported, never handed to a client as an empty message.
func (b *Box) Readable(m *mailbox.MessageMeta) bool { return Readable(b.store, m) }

// MessagePath is the file a record names.
func (b *Box) MessagePath(folder string, m *mailbox.MessageMeta) (string, error) {
	return MessagePath(b.store, folder, m)
}

// OpenMessage opens the body a record names.
func (b *Box) OpenMessage(folder string, m *mailbox.MessageMeta) (io.ReadCloser, error) {
	return OpenMessage(b.store, folder, m)
}

// RFC822Size is the size a client is told: the record's own, or the driver's
// answer from storage when it has none (#1726).
func (b *Box) RFC822Size(folder string, m *mailbox.MessageMeta) uint32 {
	return RFC822SizeOf(b.store, folder, m)
}

// FillResponseSizes fills a slice in memory for one response; nothing reaches
// disk, where the index's own StampSizes is what persists a size (#1728).
func (b *Box) FillResponseSizes(folder string, msgs []*mailbox.MessageMeta) {
	FillSizes(b.store, folder, msgs)
}

// WriteFlags settles flag changes in storage and marks those that did not
// reach it: a change kept in the index alone leaves the store stale (#1601).
func (b *Box) WriteFlags(f *mailbox.Folder, folder string, writes []mailbox.FlagWrite) []mailbox.FlagWriteResult {
	return FlagsWritten(b.index, b.store, f.ID, folder, writes)
}

// RecordSaved records a body already written into a folder — an APPEND, a
// fileinto, a copy — settling its name before its record (#1745).
func (b *Box) RecordSaved(f *mailbox.Folder, folder, saved string, m *mailbox.MessageMeta) error {
	return RecordSaved(b.index, b.store, f.ID, folder, saved, m)
}

// NameSaved settles the name of a body saved under a uid the caller already
// holds, as a delivery that reserved one does.
func (b *Box) NameSaved(folder, saved string, m *mailbox.MessageMeta) error {
	return NameSaved(b.store, folder, saved, m)
}

// MessageSize is both numbers a record reports, from the record or from the
// driver when it carries none (#1726).
func (b *Box) MessageSize(folder string, m *mailbox.MessageMeta) (size, vsize uint32, err error) {
	return MessageSize(b.store, folder, m)
}

// RemoveMessage unlinks the body a record names, leaving the record to the
// caller: an operator tool removing one is not an expunge.
func (b *Box) RemoveMessage(folder string, m *mailbox.MessageMeta) error {
	return RemoveMessage(b.store, folder, m)
}

// Messages reads records with the driver's fill-ins applied.
func (b *Box) Messages(folderID uint64, set mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	return ReadMessages(b.index, folderID, set)
}

// BoxOption tunes a box at Open time.
type BoxOption func(*Box)

// ReadOnly opens folders as they are: no adoption, no reconcile, nothing
// written. For a diagnostic, which reads an account and does not settle it.
func ReadOnly() BoxOption {
	return func(b *Box) { b.mode = openReadOnly }
}

// SaveOnly opens folders for a delivery: it adds a message and settles nothing,
// so the account is adopted at its first session instead (#1778).
func SaveOnly() BoxOption {
	return func(b *Box) { b.mode = openSaveOnly }
}

// ExpungeMarked removes messages under one hold: a folder opened between a
// record and its body holds a file the reconcile imports back (#1794).
func (b *Box) ExpungeMarked(f *mailbox.Folder, folder string, msgs []*mailbox.MessageMeta, notify mailbox.ExpungeNotify) (removed []uint32, failed int, notifyErr error) {
	err := b.HoldFolder(folder, "expunge", func() error {
		removed, failed = b.expungeEach(f, folder, msgs, notify, &notifyErr)
		return nil
	})
	if err != nil {
		slog.Error("mailbox/expunge: the folder could not be held, so nothing was removed",
			"user", b.store.Username(), "folder", folder, "err", err)
		return nil, len(msgs), notifyErr
	}
	return removed, failed, notifyErr
}

// HoldFolder runs fn under the storage's own folder hold: an option each
// protocol passes is one the next protocol forgets (#1794).
func (b *Box) HoldFolder(folder, site string, fn func() error) error {
	h, ok := mailbox.Driver(b.store).(mailbox.FolderHolder)
	if !ok {
		return fmt.Errorf("mailbox/hold: %T cannot hold a folder", mailbox.Driver(b.store))
	}
	return h.HoldFolder(folder, site, fn)
}

// expungeEach reads the name, removes the record, then the body: a stop between
// the last two leaves a file for the next rebuild, never a record with no file (#1690).
func (b *Box) expungeEach(f *mailbox.Folder, folder string, msgs []*mailbox.MessageMeta, notify mailbox.ExpungeNotify, notifyErr *error) (removed []uint32, failed int) {
	// Counted over every record in the folder, not only the doomed ones: a
	// record that stays behind still names its body (#1693).
	all, aerr := ReadMessages(b.index, f.ID, mailbox.SeqSet{})
	if aerr != nil {
		slog.Error("mailbox/expunge: the folder's records could not be read, so nothing was removed",
			"user", b.store.Username(), "folder", folder, "err", aerr)
		return nil, len(msgs)
	}
	refs := newBodyRefs(bodyNames(b.store, folder, all))
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
		if nameErr != nil {
			name = ""
		}
		switch refs.fate(name) {
		case bodyNameless:
			slog.Warn("mailbox/expunge: the record named no file; its body, if any, stays",
				"user", b.store.Username(), "folder", folder, "uid", m.UID, "err", nameErr)
		case bodyShared:
			slog.Warn("mailbox/expunge: the body stays, another record still names it",
				"user", b.store.Username(), "folder", folder, "uid", m.UID, "file", name)
		case bodyFree:
			if err := b.RemoveHeld(folder, name); err != nil {
				slog.Error("mailbox/expunge: body", "user", b.store.Username(),
					"folder", folder, "uid", m.UID, "file", name, "err", err)
			}
		}
		if notify != nil {
			if nerr := notify(m); nerr != nil {
				*notifyErr = nerr
				return removed, failed
			}
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

var _ mailbox.Box = (*Box)(nil)

// RemoveHeld unlinks a body while this folder is held: a driver whose Remove
// takes the same hold cannot be called from inside one (#1794).
func (b *Box) RemoveHeld(folder, name string) error {
	if r, ok := mailbox.Driver(b.store).(mailbox.HeldRemover); ok {
		return r.RemoveHeld(folder, name)
	}
	return b.store.Remove(folder, name)
}
