package mailbox

import "fmt"

// Box is one account's mail, both halves together: every rule needing the store
// and the index lives here, so a consumer asks for a message (#1715).
type Box struct {
	store UserMailbox
	index UserIndex
}

// Open pairs the two halves of one account.
func Open(store UserMailbox, index UserIndex) *Box {
	return &Box{store: store, index: index}
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
