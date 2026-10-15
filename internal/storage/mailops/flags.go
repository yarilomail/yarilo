package mailops

import (
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// UpdateFlags applies one change per uid to f in one transaction (#1827) and
// publishes it; the caller settles storage, after any script (#1601).
func (o *Ops) UpdateFlags(box mailbox.Box, f *mailbox.Folder, batch map[uint32]mailbox.FlagsUpdate) (map[uint32]mailbox.FlagsResult, error) {
	tx, err := box.Begin(f.ID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for uid, upd := range batch {
		tx.UpdateFlags(uid, upd)
	}
	out, err := tx.Commit()
	if err != nil {
		return nil, err
	}
	o.EmitMessage(f.Name, locks.EventChanged, 0)
	return out.Flags, nil
}
