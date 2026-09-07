package maildir

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A maildir message is found the way the reference finds it: the list says
// which uid a base name holds, and the directory says which flags it wears
// right now -- the trailer moves with every flag change (#1700).
func (u *userMailbox) RecordPath(folder string, m *mailbox.MessageMeta) (string, error) {
	if m.UID == 0 {
		return "", fmt.Errorf("maildir/by-uid: uid 0 names no message")
	}
	base, err := u.baseForUID(folder, m.UID)
	if err != nil {
		return "", err
	}
	if base == "" {
		return "", fmt.Errorf("maildir/by-uid: %q uid %d is in no list entry: %w",
			folder, m.UID, mailbox.ErrCorruptStorage)
	}
	return u.currentName(folder, base)
}

// baseForUID reads the list once and asks the map the load built, so a fetch
// over a whole folder costs one read rather than one walk per message.
func (u *userMailbox) baseForUID(folder string, uid uint32) (string, error) {
	if _, err := u.readUIDList(folder); err != nil {
		return "", fmt.Errorf("maildir/by-uid: read list %q: %w", folder, err)
	}
	fi, err := os.Stat(u.uidListPath(folder))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // no list at all: the caller reports an unnamed record
		}
		return "", fmt.Errorf("maildir/by-uid: stat list %q: %w", folder, err)
	}
	base, _ := u.folderCacheFor(folder).baseOf(uid, fi.ModTime(), fi.Size())
	return base, nil
}

// currentName finds the file a base name wears now: the flags in the trailer
// change without the base changing, which is why the list keys on the base.
func (u *userMailbox) currentName(folder, base string) (string, error) {
	entries, err := u.dirEntriesFor(folder)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if maildirBase(e.Name()) == base {
			return e.Name(), nil
		}
	}
	// fs.ErrNotExist, because that is what it is: the file the record stands
	// for is not there, which a reader tells apart from an unreadable one.
	return "", fmt.Errorf("maildir/by-uid: %q holds no file named %q: %w",
		folder, base, fs.ErrNotExist)
}

// dirEntriesFor lists cur/, through the cache the scan already keeps.
func (u *userMailbox) dirEntriesFor(folder string) ([]os.DirEntry, error) {
	dir := filepath.Join(u.folderPath(folder), "cur")
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("maildir/by-uid: stat %q: %w", folder, err)
	}
	cache := u.folderCacheFor(folder)
	if entries, ok := cache.dirEntries(st.ModTime()); ok {
		return entries, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("maildir/by-uid: list %q: %w", folder, err)
	}
	cache.storeDirEntries(entries, st.ModTime())
	return entries, nil
}

func (u *userMailbox) OpenRecord(folder string, m *mailbox.MessageMeta) (io.ReadCloser, error) {
	name, err := u.RecordPath(folder, m)
	if err != nil {
		return nil, err
	}
	return u.Fetch(folder, name, m.AltTier)
}

// basesByUID is the list read the way a record asks it, for a pass that walks
// every record of a folder.
func (u *userMailbox) basesByUID(folder string) (map[uint32]string, error) {
	uids, err := u.readUIDList(folder)
	if err != nil {
		return nil, fmt.Errorf("maildir/by-uid: read list %q: %w", folder, err)
	}
	out := make(map[uint32]string, len(uids))
	for base, uid := range uids {
		out[uid] = base
	}
	return out, nil
}

// unlistedSaid keeps one line per record per process, the way the sidecar's
// report had to learn to (#1693).
var unlistedSaid sync.Map

func reportUnlisted(user, folder string, uid uint32) {
	key := user + "\x00" + folder + "\x00" + strconv.FormatUint(uint64(uid), 10)
	if _, said := unlistedSaid.LoadOrStore(key, struct{}{}); said {
		return
	}
	slog.Error("maildir: the list names no file for this record",
		"user", user, "folder", folder, "uid", uid)
}

// SetTestFlagRenameDelay makes each flag rename sleep. Test seam for a caller
// outside this package that needs a visibly slow storage write.
func SetTestFlagRenameDelay(d time.Duration) func() {
	beforeFlagRename = func() { time.Sleep(d) }
	return func() { beforeFlagRename = nil }
}
