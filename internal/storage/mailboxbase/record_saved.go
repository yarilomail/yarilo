package mailboxbase

import (
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// RecordSaved allocates the uid and records the message; a driver named by uid
// settles the name in that cycle. saved is what Save returned (#1700).
func RecordSaved(idx mailbox.UserIndex, box mailbox.UserMailbox, folderID uint64, folder, saved string, m *mailbox.MessageMeta) error {
	// The store's UID space before the uid: a delivery reaching a store first
	// takes uids its own list already gave away otherwise (#2083).
	if al, ok := mailbox.Driver(box).(mailbox.UIDSpaceAligningStore); ok {
		if _, err := al.AlignUIDSpace(idx, folderID, folder); err != nil {
			return err
		}
	}
	stampStorageKey(box, folder, saved, m)
	namer, isNamer := mailbox.Driver(box).(mailbox.UIDNamer)
	appender, isAppender := idx.(mailbox.NamingAppender)
	if !isNamer || !isAppender {
		return idx.AllocateAndAppend(folderID, m)
	}
	return appender.AllocateAndAppendNamed(folderID, m, func(uid uint32) (string, error) {
		named, err := namer.AssignUID(folder, saved, uid)
		if err != nil {
			return "", err
		}
		stampStorageKey(box, folder, named, m)
		stampReceived(box, folder, named, m)
		return named, nil
	})
}

// NameSaved gives a saved message its name when the caller already holds the
// uid, as a delivery that reserved one does. No cycle of its own.
func NameSaved(box mailbox.UserMailbox, folder, saved string, m *mailbox.MessageMeta) error {
	stampStorageKey(box, folder, saved, m)
	namer, ok := mailbox.Driver(box).(mailbox.UIDNamer)
	if !ok {
		return nil
	}
	named, err := namer.AssignUID(folder, saved, m.UID)
	if err != nil {
		return err
	}
	stampStorageKey(box, folder, named, m)
	stampReceived(box, folder, named, m)
	return nil
}

// stampReceived dates the named file only now: an old date on a temp would
// have the temp sweep take it for a crash's leftover. A failure keeps the
// write time, the date the file had before (#2175).
func stampReceived(box mailbox.UserMailbox, folder, name string, m *mailbox.MessageMeta) {
	st, ok := mailbox.Driver(box).(mailbox.ReceivedStamper)
	if !ok || name == "" || m.InternalDate.IsZero() {
		return
	}
	if err := st.StampReceived(folder, name, m.InternalDate); err != nil {
		slog.Warn("mailbox/save: the file keeps its write time, not the INTERNALDATE",
			"user", box.Username(), "folder", folder, "file", name, "err", err)
	}
}

// stampStorageKey carries a driver's own key into the record: mdbox's map_uid,
// which is what the name is read back from.
func stampStorageKey(box mailbox.UserMailbox, folder, name string, m *mailbox.MessageMeta) {
	keyer, ok := mailbox.Driver(box).(mailbox.StorageKeyer)
	if !ok || name == "" {
		return
	}
	if mapUID, saveDate, have := keyer.StorageKey(folder, name); have {
		m.MapUID, m.SaveDate = mapUID, saveDate
	}
}
