package mailboxbase

import "github.com/yarilomail/yarilo/pkg/mailbox"

// UpdateFlags applies one delta to one message's record.
func (b *Box) UpdateFlags(folderID uint64, uid uint32, upd mailbox.FlagsUpdate) error {
	switch upd.Mode {
	case mailbox.FlagsAdd:
		return b.index.AddFlags(folderID, uid, upd.Flags, upd.Keywords)
	case mailbox.FlagsRemove:
		return b.index.RemoveFlags(folderID, uid, upd.Flags, upd.Keywords)
	default:
		return b.index.UpdateFlags(folderID, uid, upd.Flags, upd.Keywords)
	}
}

// unlockedUIDLReader reads the saved UIDLs without the cross-process lock: the
// answer goes to the client, and a read whose answer decides a write does not.
type unlockedUIDLReader interface {
	GetPOP3UIDLsUnlocked(folderID uint64) (map[uint32]string, error)
}

// POP3UIDLs returns the stable UIDLs an earlier session saved.
func (b *Box) POP3UIDLs(folderID uint64) (map[uint32]string, error) {
	if u, ok := b.index.(unlockedUIDLReader); ok {
		return u.GetPOP3UIDLsUnlocked(folderID)
	}
	return b.index.GetPOP3UIDLs(folderID)
}

// SavePOP3UIDLs persists them for the next session.
func (b *Box) SavePOP3UIDLs(folderID uint64, uidls map[uint32]string) error {
	return b.index.SavePOP3UIDLs(folderID, uidls)
}
