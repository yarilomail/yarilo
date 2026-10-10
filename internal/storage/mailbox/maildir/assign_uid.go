package maildir

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// rememberGUID keeps an explicit GUID until the message has a uid to be
// recorded against: a record with no uid names no message (#1703).
func (u *userMailbox) rememberGUID(folder, filename string, guid [16]byte) {
	u.cacheMu.Lock()
	defer u.cacheMu.Unlock()
	if u.pending == nil {
		u.pending = make(map[string][16]byte)
	}
	u.pending[folder+"\x00"+maildirBase(filename)] = guid
}

func (u *userMailbox) takeGUID(folder, filename string) ([16]byte, bool) {
	u.cacheMu.Lock()
	defer u.cacheMu.Unlock()
	key := folder + "\x00" + maildirBase(filename)
	guid, ok := u.pending[key]
	delete(u.pending, key)
	return guid, ok
}

func (u *userMailbox) rememberReceived(folder, filename string, when time.Time) {
	u.cacheMu.Lock()
	defer u.cacheMu.Unlock()
	if u.received == nil {
		u.received = make(map[string]time.Time)
	}
	u.received[folder+"\x00"+maildirBase(filename)] = when
}

func (u *userMailbox) takeReceived(folder, filename string) time.Time {
	u.cacheMu.Lock()
	defer u.cacheMu.Unlock()
	key := folder + "\x00" + maildirBase(filename)
	when := u.received[key]
	delete(u.received, key)
	return when
}

// DiscardSaved unlinks a body Save left in tmp/, where Remove does not look; one
// AssignUID already moved is removed from where it went.
func (u *userMailbox) DiscardSaved(folder, saved string, _ *mailbox.MessageMeta) error {
	u.takeGUID(folder, saved)
	u.takeReceived(folder, saved)
	err := os.Remove(filepath.Join(u.folderPath(folder), "tmp", saved))
	if errors.Is(err, os.ErrNotExist) {
		return u.Remove(folder, saved)
	}
	return err
}

// RestoreMoved renames the body back under orig, from tmp/ or from where the
// destination's naming put it.
func (u *userMailbox) RestoreMoved(srcFolder, orig, dstFolder, moved string, _ *mailbox.MessageMeta) error {
	u.takeGUID(dstFolder, moved)
	return u.withTwoMailboxLocks(srcFolder, dstFolder, lockSiteMove, func() error {
		from, ok := u.locate(dstFolder, moved)
		if !ok {
			from = filepath.Join(u.folderPath(dstFolder), "tmp", moved)
		}
		sub := "cur"
		if maildirBase(orig) == orig {
			sub = "new"
		}
		if err := os.Rename(from, filepath.Join(u.folderPath(srcFolder), sub, orig)); err != nil {
			return fmt.Errorf("maildir/restore: %w", err)
		}
		u.folderCacheFor(srcFolder).invalidateDir("own-write")
		u.folderCacheFor(dstFolder).invalidateDir("own-write")
		return nil
	})
}

// AssignUID records the message in the folder's list, inside the caller's uid
// cycle. No rename: on maildir the uid lives in the list, not in the name.
func (u *userMailbox) AssignUID(folder, filename string, uid uint32) (string, error) {
	var named string
	err := u.SaveSection(folder, func() error {
		var aerr error
		named, aerr = u.AssignUIDHeld(folder, filename, uid)
		return aerr
	})
	return named, err
}

// AssignUIDHeld is AssignUID inside a save section the caller holds.
func (u *userMailbox) AssignUIDHeld(folder, filename string, uid uint32) (string, error) {
	if uid == 0 {
		return "", fmt.Errorf("maildir/assign: uid 0 names no message")
	}
	guid, override := u.takeGUID(folder, filename)
	received := u.takeReceived(folder, filename)
	// The caller's save section holds the list and the mailbox: the file enters
	// cur/ and is named there, and its record lands before the list is let go,
	// so no other process sees the row without the record (#1736, #2184).
	if err := u.publishFromTemp(folder, filename, received); err != nil {
		return "", err
	}
	if err := u.appendUIDListLocked(folder, uid, filename, override, guid); err != nil {
		return "", err
	}
	if testAfterAssign != nil {
		testAfterAssign()
	}
	return filename, nil
}

// publishFromTemp moves a saved body out of tmp/ into the directory its name
// asks for: a name with no ":2," carries no flags, and a file that carries no
// flags belongs in new/ (#1959). A message already published is one a caller
// named twice, which is not an error to fail on.
func (u *userMailbox) publishFromTemp(folder, filename string, received time.Time) error {
	dir := u.folderPath(folder)
	src := filepath.Join(dir, "tmp", filename)
	if _, err := lstatPath(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("maildir/assign: stat temp %q: %w", filename, err)
	}
	sub := "cur"
	if maildirBase(filename) == filename {
		sub = "new"
	}
	// Dated before it is visible, so no scan caches the write time; the fresh
	// ctime this leaves keeps the temp from a sweep in any process (#2175).
	if !received.IsZero() {
		if err := os.Chtimes(src, received, received); err != nil {
			slog.Warn("maildir: the file keeps its write time, not the INTERNALDATE",
				"user", u.username, "folder", folder, "file", filename, "err", err)
		}
	}
	if err := os.Rename(src, filepath.Join(dir, sub, filename)); err != nil {
		return fmt.Errorf("maildir/assign: publish %q: %w", filename, err)
	}
	if testAfterPublish != nil {
		testAfterPublish()
	}
	// The entry, not the file: a crash here loses the name, not the bytes.
	if u.b.fsync.SyncsDir() {
		if err := syncDir(filepath.Join(dir, sub)); err != nil {
			return fmt.Errorf("maildir/assign: sync %s: %w", sub, err)
		}
	}
	u.afterPublish(folder, sub, filename)
	return nil
}

// afterPublish keeps the window over a name this process wrote. A file landing
// in new/ is not in the cur/ listing at all, so it changes nothing there.
func (u *userMailbox) afterPublish(folder, sub, name string) {
	if sub == "new" {
		return
	}
	cache := u.folderCacheFor(folder)
	dir := filepath.Join(u.folderPath(folder), "cur")
	fi, err := statPath(dir)
	if err != nil {
		cache.invalidateDirEntries("own-write")
		return
	}
	cache.addEntry(dir, name, fi.ModTime())
}

// testAfterPublish runs right after a body lands in cur/ or new/. Test seam:
// a scan in that window is what would cache the file's date.
var testAfterPublish func()

// testAfterAssign runs when the file and its row are visible and the caller
// has not yet committed the record. Test seam (#2183).
var testAfterAssign func()

// SaveSection runs a save's naming and record under the list lock, then the
// mailbox, the order every path takes them (#2184). AssignUID needs it held.
func (u *userMailbox) SaveSection(folder string, fn func() error) error {
	release, err := u.holdList(folder, lockSiteSave)
	if err != nil {
		return err
	}
	defer release()
	return u.withMailboxLockSite(folder, lockSiteSave, fn)
}
