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

// CountingBox is this box's folders opened as they are: a count reads the
// index and never needs the store settled first (#1875, quota-count.c:41).
func (b *Box) CountingBox() mailbox.Box {
	if b.mode == openReadOnly {
		return b
	}
	cp := *b
	cp.mode = openReadOnly
	return &cp
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

// ExpungeMarked removes messages under one hold and returns what went: a call
// out of the hold re-enters it and waits on itself (#1794, #1853).
func (b *Box) ExpungeMarked(f *mailbox.Folder, folder string, msgs []*mailbox.MessageMeta) (removed []*mailbox.MessageMeta, failed int, err error) {
	herr := b.HoldFolder(folder, "expunge", func() error {
		removed, failed = b.expungeEach(f, folder, msgs)
		return nil
	})
	if herr != nil {
		slog.Error("mailbox/expunge: the folder could not be held, so nothing was removed",
			"user", b.store.Username(), "folder", folder, "err", herr)
		return nil, len(msgs), herr
	}
	return removed, failed, nil
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

// expungeEach reads the names, removes the records, then the bodies: a stop
// between the last two leaves a file, never a record with no file (#1690).
func (b *Box) expungeEach(f *mailbox.Folder, folder string, msgs []*mailbox.MessageMeta) (removed []*mailbox.MessageMeta, failed int) {
	// Counted over every record in the folder, not only the doomed ones: a
	// record that stays behind still names its body (#1693).
	all, aerr := ReadMessages(b.index, f.ID, mailbox.SeqSet{})
	if aerr != nil {
		slog.Error("mailbox/expunge: the folder's records could not be read, so nothing was removed",
			"user", b.store.Username(), "folder", folder, "err", aerr)
		return nil, len(msgs)
	}
	refs := newBodyRefs(bodyNames(b.store, folder, all))

	// Every name is read before any record goes: a driver named by uid reads
	// it out of the record this command is about to remove (#1712).
	type doomed struct {
		msg  *mailbox.MessageMeta
		name string
		err  error
	}
	list := make([]doomed, 0, len(msgs))
	for _, m := range msgs {
		name, nameErr := MessagePath(b.store, folder, m)
		if nameErr != nil {
			name = ""
		}
		list = append(list, doomed{msg: m, name: name, err: nameErr})
	}

	tx, terr := b.index.Begin(f.ID)
	if terr != nil {
		slog.Error("mailbox/expunge: the folder could not be opened for writing, so nothing was removed",
			"user", b.store.Username(), "folder", folder, "err", terr)
		return nil, len(msgs)
	}
	defer tx.Rollback()
	for _, d := range list {
		tx.Expunge(d.msg.UID)
	}
	if _, cerr := tx.Commit(); cerr != nil {
		slog.Error("mailbox/expunge: the records could not be removed, so no body was",
			"user", b.store.Username(), "folder", folder, "err", cerr)
		return nil, len(msgs)
	}
	for _, d := range list {
		removed = append(removed, d.msg)
	}
	if testAfterRecordExpunged != nil {
		testAfterRecordExpunged()
	}

	// Gathered, then removed in one call: a driver that locks something
	// user-wide per body makes an expunge N round trips under the hold (#1884).
	free := make([]string, 0, len(list))
	for _, d := range list {
		switch refs.fate(d.name) {
		case bodyNameless:
			slog.Warn("mailbox/expunge: the record named no file; its body, if any, stays",
				"user", b.store.Username(), "folder", folder, "uid", d.msg.UID, "err", d.err)
		case bodyShared:
			slog.Warn("mailbox/expunge: the body stays, another record still names it",
				"user", b.store.Username(), "folder", folder, "uid", d.msg.UID, "file", d.name)
		case bodyFree:
			free = append(free, d.name)
		}
	}
	if err := b.RemoveManyHeld(folder, free); err != nil {
		slog.Error("mailbox/expunge: bodies", "user", b.store.Username(),
			"folder", folder, "count", len(free), "err", err)
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
// RemoveManyHeld unlinks every named body, in one operation where the driver
// offers it: mdbox locks the whole user's map for each one otherwise (#1884).
func (b *Box) RemoveManyHeld(folder string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	if r, ok := mailbox.Driver(b.store).(mailbox.BatchRemover); ok {
		return r.RemoveManyHeld(folder, names)
	}
	var firstErr error
	for _, name := range names {
		if err := b.RemoveHeld(folder, name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *Box) RemoveHeld(folder, name string) error {
	if r, ok := mailbox.Driver(b.store).(mailbox.HeldRemover); ok {
		return r.RemoveHeld(folder, name)
	}
	return b.store.Remove(folder, name)
}
