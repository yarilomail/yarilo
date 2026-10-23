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
	// Where the file name carries the flags, the index holds the truth until
	// the rename lands: a sync in between must not take the old name back.
	names := keepsFlagsInNames(box)
	for uid, upd := range batch {
		tx.UpdateFlags(uid, upd)
		if names {
			tx.MarkDirty(uid, true)
		}
	}
	out, err := tx.Commit()
	if err != nil {
		return nil, err
	}
	o.EmitMessage(f.Name, locks.EventChanged, 0)
	return out.Flags, nil
}

func keepsFlagsInNames(box mailbox.Box) bool {
	switch mailbox.Driver(box.Store()).(type) {
	case mailbox.FlagWriterMulti, mailbox.FlagWriter:
		return true
	}
	return false
}
