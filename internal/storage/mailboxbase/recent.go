package mailboxbase

import "github.com/yarilomail/yarilo/v2/pkg/mailbox"

// ClaimRecent takes f's \Recent messages for the caller.
func (b *Box) ClaimRecent(f *mailbox.Folder) (from, to uint32, err error) {
	rc, ok := b.index.(mailbox.RecentClaimer)
	if !ok {
		return f.NextUID, f.NextUID, nil
	}
	return rc.ClaimRecent(f.ID)
}
