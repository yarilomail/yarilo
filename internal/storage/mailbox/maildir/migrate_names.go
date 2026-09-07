package maildir

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// MigrateUIDNames puts every record's name in the list, which is where a
// maildir message is found from its uid. Two sources: the sidecar an older
// build wrote, and, where that is gone, the guid the base name derives (#1726).
//
// The sidecar is removed only once every record it named is in the list.
func (u *userMailbox) MigrateUIDNames(idx mailbox.UserIndex, folder *mailbox.Folder) (int, error) {
	marker, ok := idx.(mailbox.UIDNameMarker)
	if !ok {
		return 0, fmt.Errorf("maildir/migrate: %q: the index cannot record the pass", folder.Name)
	}
	switch done, err := marker.UIDNamed(folder.ID); {
	case err != nil:
		return 0, fmt.Errorf("maildir/migrate: read marker %q: %w", folder.Name, err)
	case done:
		return 0, nil
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		return 0, fmt.Errorf("maildir/migrate: get messages %q: %w", folder.Name, err)
	}
	stored := u.storedNames(idx, folder)

	placed := 0
	var unresolved []uint32
	err = u.withMailboxLockSite(folder.Name, lockSiteMigrateNames, func() error {
		known, kerr := u.basesByUID(folder.Name)
		if kerr != nil {
			return kerr
		}
		byGUID := u.basesByGUID(folder.Name)
		place := map[uint32]string{}
		for _, m := range msgs {
			if _, listed := known[m.UID]; listed {
				continue
			}
			switch base, from := u.baseFor(m, stored, byGUID); from {
			case "":
				unresolved = append(unresolved, m.UID)
			default:
				place[m.UID] = base
			}
		}
		if len(place) == 0 {
			return nil
		}
		n, perr := u.placeUIDsLocked(folder.Name, place)
		placed = n
		return perr
	})
	if err != nil {
		return placed, err
	}
	for _, uid := range unresolved {
		reportUnplaced(u.username, folder.Name, uid)
	}
	if len(stored) > 0 {
		if left := u.storedNotListed(folder.Name, stored); left > 0 {
			// Not removed, not marked: the file still holds the only name for
			// those records, and the next open must try again (#1713, #1726).
			slog.Warn("maildir: the sidecar still names records the list does not",
				"user", u.username, "folder", folder.Name, "left", left, "placed", placed)
			return placed, nil
		}
		if forgetter, canForget := idx.(mailbox.StoredNameForgetter); canForget {
			if ferr := forgetter.ForgetStoredNames(folder.ID); ferr != nil {
				return placed, ferr
			}
		}
	}
	if placed > 0 {
		slog.Info("maildir: records took the name their file gives them",
			"user", u.username, "folder", folder.Name, "placed", placed, "unresolved", len(unresolved))
	}
	return placed, marker.MarkUIDNamed(folder.ID)
}

// baseFor is where a record's name comes from: the sidecar first, since it was
// written when the name was known, then the guid the base derives.
func (u *userMailbox) baseFor(m *mailbox.MessageMeta, stored map[uint32]string, byGUID map[[16]byte]string) (string, string) {
	if name, ok := stored[m.UID]; ok && name != "" {
		return maildirBase(name), "sidecar"
	}
	if m.GUID != ([16]byte{}) {
		if base, ok := byGUID[m.GUID]; ok {
			return base, "guid"
		}
	}
	return "", ""
}

// basesByGUID indexes cur/ by the guid each base derives. A record whose guid
// was overridden at save is not in here: its guid lived in the list entry that
// is gone, so the pass reports it rather than guessing a file for it (#1726).
func (u *userMailbox) basesByGUID(folder string) map[[16]byte]string {
	entries, err := u.dirEntriesFor(folder)
	if err != nil {
		return nil
	}
	out := make(map[[16]byte]string, len(entries))
	for _, e := range entries {
		base := maildirBase(e.Name())
		out[guidFromBase(base)] = base
	}
	return out
}

// storedNames is what the sidecar holds, or nothing when it is already gone.
func (u *userMailbox) storedNames(idx mailbox.UserIndex, folder *mailbox.Folder) map[uint32]string {
	lister, ok := idx.(mailbox.StoredNameLister)
	if !ok || !u.sidecarPresent(idx, folder) {
		return nil
	}
	names, err := lister.StoredNames(folder.ID)
	if err != nil {
		slog.Warn("maildir: the sidecar could not be read",
			"user", u.username, "folder", folder.Name, "err", err)
		return nil
	}
	return names
}

// storedNotListed counts the sidecar's records the list still does not name.
func (u *userMailbox) storedNotListed(folder string, stored map[uint32]string) int {
	known, err := u.basesByUID(folder)
	if err != nil {
		return len(stored)
	}
	left := 0
	for uid, name := range stored {
		if name == "" {
			continue
		}
		if _, listed := known[uid]; !listed {
			left++
		}
	}
	return left
}

// sidecarPresent is one stat, so an open costs nothing once the file is gone.
func (u *userMailbox) sidecarPresent(idx mailbox.UserIndex, folder *mailbox.Folder) bool {
	dir, ok := idx.(interface{ IndexDirFor(string) string })
	if !ok {
		return false
	}
	_, err := os.Stat(filepath.Join(dir.IndexDirFor(folder.Name), indexNamesFileName))
	return err == nil
}

// indexNamesFileName is the sidecar's name, spelled here so this driver can ask
// whether one is there without reaching into the index package.
const indexNamesFileName = "yarilo.index.names"

// unplacedSaid keeps one line per record per process.
var unplacedSaid sync.Map

func reportUnplaced(user, folder string, uid uint32) {
	key := user + "\x00" + folder + "\x00" + strconv.FormatUint(uint64(uid), 10)
	if _, said := unplacedSaid.LoadOrStore(key, struct{}{}); said {
		return
	}
	slog.Error("maildir: no file in this folder answers for the record",
		"user", user, "folder", folder, "uid", uid)
}
