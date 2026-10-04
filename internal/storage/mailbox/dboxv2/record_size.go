package dboxv2

import (
	"path/filepath"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// RecordSize answers from the message's own dbox metadata, counting the body
// when that says nothing: a converted store's records say nothing (#1728).
func (u *userMailbox) RecordSize(folder string, m *mailbox.MessageMeta) (uint32, uint32, error) {
	if m.Size != 0 && m.VSize != 0 {
		return m.Size, m.VSize, nil
	}
	name, err := u.RecordPath(folder, m)
	if err != nil {
		return m.Size, m.VSize, err
	}
	path := filepath.Join(u.folderPath(folder), name)
	if _, vsize, _, merr := readMetadata(path); merr == nil && vsize != 0 {
		size := m.Size
		if size == 0 {
			size = vsize
		}
		return size, vsize, nil
	}
	rc, oerr := u.OpenRecord(folder, m)
	if oerr != nil {
		return m.Size, m.VSize, oerr
	}
	defer rc.Close() //nolint:errcheck
	return mailboxbase.CountSizes(rc)
}

var _ mailbox.RecordSizer = (*userMailbox)(nil)
