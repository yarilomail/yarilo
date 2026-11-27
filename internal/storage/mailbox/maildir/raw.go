package maildir

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// RawRecord opens the file a record names, as stored: no decryption, no CRLF.
func (u *userMailbox) RawRecord(folder string, m *mailbox.MessageMeta) (mailbox.RawBody, error) {
	b, err := u.rawRecord(folder, m)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return b, err
	}
	// A STORE renamed the file between the lookup and the open (OpenRecord).
	u.folderCacheFor(folder).invalidateDirEntries("open")
	return u.rawRecord(folder, m)
}

func (u *userMailbox) rawRecord(folder string, m *mailbox.MessageMeta) (mailbox.RawBody, error) {
	name, err := u.RecordPath(folder, m)
	if err != nil {
		return nil, err
	}
	if testBeforeRawLocate != nil {
		testBeforeRawLocate()
	}
	p, ok := u.locate(folder, name)
	if !ok {
		return nil, fmt.Errorf("maildir: raw %s: %w", name, fs.ErrNotExist)
	}
	f, err := openPath(p)
	if err != nil {
		return nil, fmt.Errorf("maildir: raw %s: %w", name, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("maildir: raw %s: %w", name, err)
	}
	return fileBody{File: f, size: st.Size()}, nil
}

// testBeforeRawLocate runs between a record's name lookup and its open. Test
// seam: a STORE renames the file there.
var testBeforeRawLocate func()
