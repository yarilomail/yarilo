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

// MigrateUIDNames puts every record's name in the list: from the sidecar, then
// from the guid a base name derives. The sidecar goes once all of it is in (#1726).
func (u *userMailbox) MigrateUIDNames(box mailbox.Box, folder *mailbox.Folder) (int, error) {
	marker, ok := box.Index().(mailbox.UIDNameMarker)
	if !ok {
		return 0, fmt.Errorf("maildir/migrate: %q: the index cannot record the pass", folder.Name)
	}
	switch done, err := marker.UIDNamed(folder.ID); {
	case err != nil:
		return 0, fmt.Errorf("maildir/migrate: read marker %q: %w", folder.Name, err)
	case done:
		return 0, nil
	}
	msgs, err := box.Index().GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		return 0, fmt.Errorf("maildir/migrate: get messages %q: %w", folder.Name, err)
	}
	stored := u.storedNames(box, folder)

	placed := 0
	var unresolved []uint32
	vsizes := map[uint32]uint32{}
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
		n, taken, perr := u.placeUIDsLocked(folder.Name, place)
		placed = n
		unresolved = append(unresolved, taken...)
		if perr != nil {
			return perr
		}
		for uid, base := range place {
			if _, refused := refusedSet(taken)[uid]; refused {
				continue
			}
			if _, vsize, ok := sizesFromName(base); ok {
				vsizes[uid] = vsize
				continue
			}
			if _, vsize, merr := measureSizes(filepath.Join(u.folderPath(folder.Name), "cur", base)); merr == nil {
				vsizes[uid] = vsize
			}
		}
		return nil
	})
	if err != nil {
		return placed, err
	}
	for _, uid := range unresolved {
		reportUnplaced(u.username, folder.Name, uid)
	}
	// The size was kept beside the name and went with it; quota sums the record,
	// so a folder recovered without it is counted as empty (#1728).
	if stamper, canStamp := box.Index().(mailbox.SizeStamper); canStamp && len(vsizes) > 0 {
		if _, serr := stamper.StampSizes(folder.ID, vsizes); serr != nil {
			return placed, serr
		}
	}
	if len(stored) > 0 {
		if left := u.storedNotListed(folder.Name, stored); left > 0 {
			// Not removed, not marked: the file still holds the only name for
			// those records, and the next open must try again (#1713, #1726).
			slog.Warn("maildir: the sidecar still names records the list does not",
				"user", u.username, "folder", folder.Name, "left", left, "placed", placed)
			return placed, nil
		}
		if forgetter, canForget := box.Index().(mailbox.StoredNameForgetter); canForget {
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

// basesByGUID indexes cur/ by the guid each base derives. An overridden guid
// lived in the lost list entry, so such a record is reported, not guessed (#1726).
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
func (u *userMailbox) storedNames(box mailbox.Box, folder *mailbox.Folder) map[uint32]string {
	lister, ok := box.Index().(mailbox.StoredNameLister)
	if !ok || !u.sidecarPresent(box, folder) {
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
func (u *userMailbox) sidecarPresent(box mailbox.Box, folder *mailbox.Folder) bool {
	dir, ok := box.Index().(interface{ IndexDirFor(string) string })
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

// refusedSet is the uids placeUIDsLocked would not take a file for.
func refusedSet(uids []uint32) map[uint32]struct{} {
	out := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		out[uid] = struct{}{}
	}
	return out
}
