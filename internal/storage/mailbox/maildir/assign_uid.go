package maildir

import (
	"fmt"
	"os"
	"path/filepath"
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

// AssignUID records the message in the folder's list, inside the caller's uid
// cycle. No rename: on maildir the uid lives in the list, not in the name.
func (u *userMailbox) AssignUID(folder, filename string, uid uint32) (string, error) {
	if uid == 0 {
		return "", fmt.Errorf("maildir/assign: uid 0 names no message")
	}
	guid, override := u.takeGUID(folder, filename)
	if err := u.withMailboxLockSite(folder, lockSiteSave, func() error {
		// The file enters cur/ here and leaves this hold already named, which is
		// why the reference takes the list lock before moving out of tmp/ (#1736).
		if err := u.publishFromTemp(folder, filename); err != nil {
			return err
		}
		return u.appendUIDListLocked(folder, uid, filename, override, guid)
	}); err != nil {
		return "", err
	}
	return filename, nil
}

// publishFromTemp moves a saved body out of tmp/ into cur/. A message already
// there is one a caller named twice, which is not an error to fail on.
func (u *userMailbox) publishFromTemp(folder, filename string) error {
	dir := u.folderPath(folder)
	src := filepath.Join(dir, "tmp", filename)
	if _, err := os.Lstat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("maildir/assign: stat temp %q: %w", filename, err)
	}
	if err := os.Rename(src, filepath.Join(dir, "cur", filename)); err != nil {
		return fmt.Errorf("maildir/assign: publish %q: %w", filename, err)
	}
	u.folderCacheFor(folder).invalidateDir()
	return nil
}
