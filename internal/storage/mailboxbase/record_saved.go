package mailboxbase

import (
	"log/slog"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// RecordSaved allocates the uid and records the message; a driver named by uid
// settles the name in that cycle. saved is what Save returned (#1700).
func RecordSaved(idx mailbox.UserIndex, box mailbox.UserMailbox, folderID uint64, folder, saved string, m *mailbox.MessageMeta) error {
	_, err := RecordSavedNamed(idx, box, folderID, folder, saved, m)
	return err
}

// RecordSavedNamed is RecordSaved answering with the name the cycle gave the
// file, so the caller need not ask the driver for it (#2639); empty when the
// driver names no file by uid.
func RecordSavedNamed(idx mailbox.UserIndex, box mailbox.UserMailbox, folderID uint64, folder, saved string, m *mailbox.MessageMeta) (string, error) {
	stampStorageKey(box, folder, saved, m)
	namer, isNamer := mailbox.Driver(box).(mailbox.UIDNamer)
	appender, isAppender := idx.(mailbox.NamingAppender)
	if !isNamer || !isAppender {
		if al, ok := mailbox.Driver(box).(mailbox.UIDSpaceAligningStore); ok {
			if _, err := al.AlignUIDSpace(idx, folderID, folder); err != nil {
				return "", err
			}
		}
		return "", idx.AllocateAndAppend(folderID, m)
	}
	var final string
	name := func(uid uint32) (string, *mailbox.MaildirStamp, error) {
		named, err := assignHeld(box, namer, folder, saved, uid)
		if err != nil {
			return "", nil, err
		}
		stampStorageKey(box, folder, named, m)
		stampReceived(box, folder, named, m)
		final = named
		// The list the row left, for the index to record in the same hold:
		// the next save takes its UID space from the index (#2643).
		if ls, ok := mailbox.Driver(box).(mailbox.AppendedListStamper); ok {
			if s, have := ls.AppendedListStamp(folder); have {
				return named, &s, nil
			}
		}
		return named, nil, nil
	}
	err := inSaveSection(box, folder, func() error {
		// The store's UID space before the uid, in the section that hands it
		// out: its rows first, then its next uid (#2083, #2663).
		if al, ok := mailbox.Driver(box).(mailbox.UIDSpaceAligningStore); ok {
			if _, err := al.AlignUIDSpaceHeld(idx, folderID, folder); err != nil {
				return err
			}
		}
		if sa, ok := idx.(mailbox.StampingAppender); ok {
			return sa.AllocateAndAppendStamped(folderID, m, name)
		}
		return appender.AllocateAndAppendNamed(folderID, m, func(uid uint32) (string, error) {
			named, _, err := name(uid)
			return named, err
		})
	})
	if err != nil {
		return "", err
	}
	return final, nil
}

// inSaveSection runs fn under the store's save section when it has one.
func inSaveSection(box mailbox.UserMailbox, folder string, fn func() error) error {
	if sec, ok := mailbox.Driver(box).(mailbox.SaveSectioner); ok {
		return sec.SaveSection(folder, fn)
	}
	return fn()
}

// NameSaved gives a saved message its name when the caller already holds the
// uid, as a delivery that reserved one does. No cycle of its own.
func NameSaved(box mailbox.UserMailbox, folder, saved string, m *mailbox.MessageMeta) error {
	return inSaveSection(box, folder, func() error { return nameSavedHeld(box, folder, saved, m) })
}

// NameAndAppend names a saved message whose uid the caller holds and appends
// its record in one save section: the row is never seen without the record.
func NameAndAppend(idx mailbox.UserIndex, box mailbox.UserMailbox, folderID uint64, folder, saved string, m *mailbox.MessageMeta) error {
	return inSaveSection(box, folder, func() error {
		if err := nameSavedHeld(box, folder, saved, m); err != nil {
			return err
		}
		return idx.AppendMessage(folderID, m)
	})
}

func nameSavedHeld(box mailbox.UserMailbox, folder, saved string, m *mailbox.MessageMeta) error {
	stampStorageKey(box, folder, saved, m)
	namer, ok := mailbox.Driver(box).(mailbox.UIDNamer)
	if !ok {
		return nil
	}
	named, err := assignHeld(box, namer, folder, saved, m.UID)
	if err != nil {
		return err
	}
	stampStorageKey(box, folder, named, m)
	stampReceived(box, folder, named, m)
	return nil
}

// assignHeld names inside the section inSaveSection opened, when there is one.
func assignHeld(box mailbox.UserMailbox, namer mailbox.UIDNamer, folder, saved string, uid uint32) (string, error) {
	if sec, ok := mailbox.Driver(box).(mailbox.SaveSectioner); ok {
		return sec.AssignUIDHeld(folder, saved, uid)
	}
	return namer.AssignUID(folder, saved, uid)
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
