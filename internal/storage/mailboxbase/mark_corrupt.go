package mailboxbase

import (
	"errors"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// MarkCorruptOnFetchErr flags a folder when err wraps ErrCorruptStorage, and
// only for a driver that heals: else the folder is stranded (#1715).
func (b *Box) MarkCorruptOnFetchErr(folder string, err error) bool {
	if err == nil || !errors.Is(err, mailbox.ErrCorruptStorage) {
		return false
	}
	if !mailbox.CanReactiveHeal(b.store) {
		return false
	}
	cm, ok := b.index.(mailbox.CorruptionMarker)
	if !ok {
		return false
	}
	f, oerr := b.Folder(folder, 0)
	if oerr != nil {
		return false
	}
	return cm.MarkFolderCorrupt(f.ID) == nil
}

// HealCorrupt repairs a folder a driver marked, with the index the base owns.
// A driver that does not heal answers nothing, which is not an error.
func (b *Box) HealCorrupt(f *mailbox.Folder) ([]uint32, error) {
	rb, ok := mailbox.Driver(b.store).(mailbox.ReactiveHealer)
	if !ok {
		return nil, nil
	}
	return rb.HealCorruptFolder(b, b.index, f)
}
