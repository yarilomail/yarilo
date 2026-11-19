package maildir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/filelock"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// rewritePrefix names a rewrite's temp file in the folder's tmp/, apart from
// the names a save puts there.
const rewritePrefix = ".yarilo-reencrypt."

var _ mailbox.RecordRewriter = (*userMailbox)(nil)

// rewriteSeq keeps two rewrites of one process apart in tmp/.
var rewriteSeq atomic.Uint64

// Test seams: after the temp is verified, and under the list just before the
// rename.
var (
	testAfterRewriteVerified func()
	testBeforeRewriteRename  func()
)

// RewriteRecord replaces a message's file under the name it wears at that
// moment: the folder's crypt lock exclusive, then the mutex, the record asked
// for in the index and the name looked up under them, so a STORE, expunge or
// move either ran before and is followed, or waits. It creates no file.
func (u *userMailbox) RewriteRecord(folder string, m *mailbox.MessageMeta, rw mailbox.BodyRewrite) (err error) {
	raw, err := u.RawRecord(folder, m)
	if err != nil {
		// Expunged only when the index says so: a file renamed twice while it
		// was looked up is gone from its name, not from the folder, and a
		// rerun takes it.
		switch {
		case stillRecorded(rw.Recorded) == errGone:
			return fmt.Errorf("maildir/rewrite: %w: %w", mailbox.ErrExpunged, err)
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("maildir/rewrite: %w: %w", mailbox.ErrRecordChanged, err)
		}
		return err
	}
	defer raw.Close() //nolint:errcheck
	src, ok := raw.(fileBody)
	if !ok {
		return fmt.Errorf("maildir/rewrite: raw record is a %T, not a file", raw)
	}
	before, err := src.Stat()
	if err != nil {
		return fmt.Errorf("maildir/rewrite: %w", err)
	}
	base := maildirBase(filepath.Base(src.Name()))

	tmp := filepath.Join(u.folderPath(folder), "tmp", fmt.Sprintf("%s%d.%d", rewritePrefix, u.b.pid, rewriteSeq.Add(1)))
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("maildir/rewrite: %w", err)
	}
	if _, _, err = rw.Fill(raw, f); err == nil {
		err = syncFile(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("maildir/rewrite %s: %w", base, err)
	}
	if err = verifyTemp(tmp, rw.Verify); err != nil {
		return fmt.Errorf("maildir/rewrite %s: %w", base, err)
	}
	if testAfterRewriteVerified != nil {
		testAfterRewriteVerified()
	}

	release, err := u.exclusiveCrypt(folder)
	switch {
	case errors.Is(err, filelock.ErrBusy):
		return fmt.Errorf("maildir/rewrite %s: %w", base, mailbox.ErrRecordBusy)
	case err != nil:
		return fmt.Errorf("maildir/rewrite %s: %w", base, err)
	}
	defer release()
	return u.withMailboxLockSite(folder, lockSiteRewrite, func() error {
		return u.swapHeld(folder, base, tmp, before, rw.Recorded)
	})
}

func (u *userMailbox) swapHeld(folder, base, tmp string, before os.FileInfo, recorded func() (bool, error)) error {
	if recorded != nil {
		ok, err := recorded()
		if err != nil {
			return fmt.Errorf("maildir/rewrite %s: index: %w", base, err)
		}
		if !ok {
			return fmt.Errorf("maildir/rewrite %s: %w", base, mailbox.ErrExpunged)
		}
	}
	u.folderCacheFor(folder).invalidateDirEntries("rewrite")
	name, _, err := u.currentName(folder, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("maildir/rewrite %s: %w", base, goneOrChanged(recorded))
		}
		return fmt.Errorf("maildir/rewrite %s: %w", base, err)
	}
	path, ok := u.locate(folder, name)
	if !ok {
		return fmt.Errorf("maildir/rewrite %s: %w", base, goneOrChanged(recorded))
	}
	now, err := statPath(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("maildir/rewrite %s: %w", base, goneOrChanged(recorded))
	case err != nil:
		return fmt.Errorf("maildir/rewrite %s: %w", base, err)
	case !os.SameFile(now, before) || now.Size() != before.Size():
		// A flag rename keeps the inode; anything else is another file.
		return fmt.Errorf("maildir/rewrite %s: %w", base, mailbox.ErrRecordChanged)
	}
	if testBeforeRewriteRename != nil {
		testBeforeRewriteRename()
	}
	// The received date is the file's mtime: set here, so until now the temp
	// is as young as it is and the sweep leaves it.
	if err := os.Chtimes(tmp, now.ModTime(), now.ModTime()); err != nil {
		return fmt.Errorf("maildir/rewrite %s: %w", base, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("maildir/rewrite %s: %w", base, err)
	}
	u.folderCacheFor(folder).invalidateDir("own-write")
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("maildir/rewrite %s: sync dir: %w", base, err)
	}
	return nil
}

var errGone = errors.New("maildir/rewrite: no longer recorded")

// goneOrChanged names a file missing from where it was looked for: expunged
// when the index has no record either, or when there is no index to ask;
// changed otherwise, for a rerun to take.
func goneOrChanged(recorded func() (bool, error)) error {
	if recorded == nil || stillRecorded(recorded) == errGone {
		return mailbox.ErrExpunged
	}
	return mailbox.ErrRecordChanged
}

func stillRecorded(recorded func() (bool, error)) error {
	if recorded == nil {
		return nil
	}
	if ok, err := recorded(); err == nil && !ok {
		return errGone
	}
	return nil
}

func verifyTemp(path string, verify func(mailbox.RawBody) error) error {
	f, err := openPath(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return verify(fileBody{File: f, size: st.Size()})
}

// SweepRewriteTemps removes rewrite temps in tmp/ older than age.
func (u *userMailbox) SweepRewriteTemps(folder string, age time.Duration) (int, error) {
	dir := filepath.Join(u.folderPath(folder), "tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("maildir/sweep-rewrite: %w", err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), rewritePrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < age {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return n, fmt.Errorf("maildir/sweep-rewrite: %w", err)
		}
		n++
	}
	return n, nil
}
