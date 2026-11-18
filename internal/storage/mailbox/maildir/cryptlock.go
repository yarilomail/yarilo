package maildir

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/filelock"
)

// cryptLockName is the folder's re-encryption lock, beside its uid list
// (MAILCRYPT section 13.5).
const cryptLockName = "yarilo-crypt.lock"

// swapLockWait bounds the swap's wait: flock queues nobody, so under a stream
// of shared holds the swap gives way, and the message is left for a rerun.
var swapLockWait = 2 * time.Second

func (u *userMailbox) cryptLockPath(folder string) string {
	return filepath.Join(filepath.Dir(u.uidListPath(folder)), cryptLockName)
}

// sharedCrypt holds the folder's crypt lock shared for a rename or an unlink
// of a message file. A volume without flock holds nothing: no swap runs there.
func (u *userMailbox) sharedCrypt(folder, site string) (func(), error) {
	forget := u.noteHold("crypt:"+folder, site)
	h, err := filelock.TakeMode(u.cryptLockPath(folder), u.b.lockMethod, filelock.Shared, uidListLockWait)
	switch {
	case errors.Is(err, filelock.ErrNoSharedLock):
		forget()
		return func() {}, nil
	case err != nil:
		forget()
		return nil, fmt.Errorf("maildir: %s: crypt lock: %w", site, err)
	}
	metricCryptLock.WithLabelValues("shared", strconv.FormatBool(h.Waited)).Inc()
	return func() { _ = h.Release(); forget() }, nil
}

// exclusiveCrypt holds it exclusive for a swap; a volume without flock is an
// error, since the sessions' renames would not be kept out.
func (u *userMailbox) exclusiveCrypt(folder string) (func(), error) {
	forget := u.noteHold("crypt:"+folder, lockSiteRewrite)
	h, err := filelock.TakeMode(u.cryptLockPath(folder), u.b.lockMethod, filelock.Exclusive, swapLockWait)
	if err != nil {
		forget()
		return nil, err
	}
	metricCryptLock.WithLabelValues("exclusive", strconv.FormatBool(h.Waited)).Inc()
	return func() { _ = h.Release(); forget() }, nil
}
