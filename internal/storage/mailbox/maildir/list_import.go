package maildir

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// importListRowsHeld appends, in uid order, the list's rows at or above the
// index's next uid whose files are on disk. The caller holds the list and the
// mailbox, and moves the next uid to listNext after (#2663).
func (u *userMailbox) importListRowsHeld(idx mailbox.UserIndex, folderID uint64, folder string, listNext uint32) error {
	nr, ok := idx.(mailbox.NextUIDReader)
	if !ok || listNext == 0 {
		return nil
	}
	next, err := nr.NextUIDHeld(folderID, listNext)
	if err != nil {
		return fmt.Errorf("maildir: import list rows: %w", err)
	}
	if listNext <= next {
		return nil
	}
	rows, err := u.readUIDList(folder)
	if err != nil {
		return fmt.Errorf("maildir: import list rows: %w", err)
	}
	type row struct {
		uid  uint32
		name string
	}
	var take []row
	for name, uid := range rows {
		if uid >= next {
			take = append(take, row{uid, name})
		}
	}
	if len(take) == 0 {
		return nil
	}
	sort.Slice(take, func(i, j int) bool { return take[i].uid < take[j].uid })
	kwNames := u.keywordNames(folder)
	for _, r := range take {
		path, found := u.locate(folder, r.name)
		name := r.name
		if !found {
			cur, have, _ := u.currentName(folder, maildirBase(r.name))
			if !have {
				continue
			}
			if path, found = u.locate(folder, cur); !found {
				continue
			}
			name = cur
		}
		rec, ok := u.scanFile(folder, path, name, kwNames)
		if !ok {
			continue
		}
		m := &mailbox.MessageMeta{
			UID: r.uid, Size: rec.Size, VSize: rec.VSize, InternalDate: rec.InternalDate,
			Flags: rec.Flags, Keywords: rec.Keywords, GUID: rec.GUID,
		}
		if err := idx.AppendMessage(folderID, m); err != nil &&
			!errors.Is(err, mailbox.ErrUIDInUse) && !errors.Is(err, mailbox.ErrUIDNotIssued) {
			return fmt.Errorf("maildir: import %s at its listed uid %d: %w", filepath.Base(path), r.uid, err)
		}
	}
	return nil
}
