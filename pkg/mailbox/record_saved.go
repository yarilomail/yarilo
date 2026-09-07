package mailbox

// RecordSaved allocates the uid and records the message; a driver named by uid
// settles the name in that cycle. saved is what Save returned (#1700).
func RecordSaved(idx UserIndex, box UserMailbox, folderID uint64, folder, saved string, m *MessageMeta) error {
	stampStorageKey(box, folder, saved, m)
	namer, isNamer := Driver(box).(UIDNamer)
	appender, isAppender := idx.(NamingAppender)
	if !isNamer || !isAppender {
		return idx.AllocateAndAppend(folderID, m)
	}
	return appender.AllocateAndAppendNamed(folderID, m, func(uid uint32) (string, error) {
		named, err := namer.AssignUID(folder, saved, uid)
		if err != nil {
			return "", err
		}
		stampStorageKey(box, folder, named, m)
		return named, nil
	})
}

// NameSaved gives a saved message its name when the caller already holds the
// uid, as a delivery that reserved one does. No cycle of its own.
func NameSaved(box UserMailbox, folder, saved string, m *MessageMeta) error {
	stampStorageKey(box, folder, saved, m)
	namer, ok := Driver(box).(UIDNamer)
	if !ok {
		return nil
	}
	named, err := namer.AssignUID(folder, saved, m.UID)
	if err != nil {
		return err
	}
	stampStorageKey(box, folder, named, m)
	return nil
}

// stampStorageKey carries a driver's own key into the record: mdbox's map_uid,
// which is what the name is read back from.
func stampStorageKey(box UserMailbox, folder, name string, m *MessageMeta) {
	keyer, ok := Driver(box).(StorageKeyer)
	if !ok || name == "" {
		return
	}
	if mapUID, saveDate, have := keyer.StorageKey(folder, name); have {
		m.MapUID, m.SaveDate = mapUID, saveDate
	}
}
