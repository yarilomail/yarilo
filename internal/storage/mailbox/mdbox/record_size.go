package mdbox

import "github.com/yarilomail/yarilo/pkg/mailbox"

// RecordSize counts the message: the map holds the record's physical extent,
// not the octets a client is sent (#1728).
func (u *userMailbox) RecordSize(folder string, m *mailbox.MessageMeta) (uint32, uint32, error) {
	if m.Size != 0 && m.VSize != 0 {
		return m.Size, m.VSize, nil
	}
	rc, err := u.OpenRecord(folder, m)
	if err != nil {
		return m.Size, m.VSize, err
	}
	defer rc.Close() //nolint:errcheck
	return mailbox.CountSizes(rc)
}

var _ mailbox.RecordSizer = (*userMailbox)(nil)
