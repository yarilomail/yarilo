package maildir

import (
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// fillMissingSizes gives records carrying no size the one their name holds. The
// index sums what the records say, so a record that says nothing costs the
// folder its quota until this runs (#1728).
func (u *userMailbox) fillMissingSizes(idx mailbox.UserIndex, folder *mailbox.Folder, scanned []mailbox.ScanRecord) {
	lister, canList := idx.(mailbox.SizelessLister)
	stamper, canStamp := idx.(mailbox.SizeStamper)
	if !canList || !canStamp {
		return
	}
	uids, err := lister.SizelessUIDs(folder.ID)
	if err != nil || len(uids) == 0 {
		return
	}
	bases, err := u.basesByUID(folder.Name)
	if err != nil {
		return
	}
	// The scan already read the sizes off the names, so nothing here opens a
	// file the walk did not already stat.
	byBase := make(map[string]mailbox.ScanRecord, len(scanned))
	for _, sr := range scanned {
		byBase[maildirBase(sr.Filename)] = sr
	}
	vsizes := make(map[uint32]uint32, len(uids))
	for _, uid := range uids {
		base, listed := bases[uid]
		if !listed {
			continue
		}
		if sr, found := byBase[base]; found && sr.RFC822Size() != 0 {
			vsizes[uid] = sr.RFC822Size()
		}
	}
	if len(vsizes) == 0 {
		return
	}
	n, serr := stamper.StampSizes(folder.ID, vsizes)
	if serr != nil {
		slog.Warn("maildir: sizes not written back",
			"user", u.username, "folder", folder.Name, "err", serr)
		return
	}
	slog.Info("maildir: records took the size their name holds",
		"user", u.username, "folder", folder.Name, "filled", n, "sizeless", len(uids))
}
