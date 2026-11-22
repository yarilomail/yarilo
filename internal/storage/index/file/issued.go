package file

import (
	"fmt"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

func (u *userIndex) issue(folderID uint64, uid uint32) {
	u.issuedMu.Lock()
	defer u.issuedMu.Unlock()
	if u.issued == nil {
		u.issued = make(map[issuedUID]struct{})
	}
	u.issued[issuedUID{folderID, uid}] = struct{}{}
}

// ReleaseUID drops a uid AllocateUID handed out whose record will not come.
func (u *userIndex) ReleaseUID(folderID uint64, uid uint32) {
	u.issuedMu.Lock()
	defer u.issuedMu.Unlock()
	delete(u.issued, issuedUID{folderID, uid})
}

// claimIssued lets a record below the folder's next uid in only once, and only
// when this process handed that uid out: any other is a uid the folder gave up,
// and a record under it would bring an expunged message back (#2663).
func (u *userIndex) claimIssued(fs *folderState, folderID uint64, uid uint32) error {
	if uid == 0 || uid >= fs.file.Header.NextUID {
		return nil
	}
	k := issuedUID{folderID, uid}
	u.issuedMu.Lock()
	defer u.issuedMu.Unlock()
	if _, ok := u.issued[k]; !ok {
		if taken, err := fs.uidTaken(uid); err != nil {
			return err
		} else if taken {
			return fmt.Errorf("fileindex/append: uid %d: %w", uid, mailbox.ErrUIDInUse)
		}
		return fmt.Errorf("fileindex/append: uid %d, next %d: %w", uid, fs.file.Header.NextUID, mailbox.ErrUIDNotIssued)
	}
	delete(u.issued, k)
	return nil
}

// NextUIDHeld reads the folder's next uid as its writer holds it, taking the
// lock only when an unlocked read is below want: the counter never goes down.
func (u *userIndex) NextUIDHeld(folderID uint64, want uint32) (uint32, error) {
	var next uint32
	if err := u.withFolderHeaderUnlocked(folderID, func(fs *folderState) error {
		next = fs.file.Header.NextUID
		return nil
	}); err != nil || next >= want {
		return next, err
	}
	err := u.withFolderSite(folderID, lockSiteAllocateUid, func(fs *folderState) error {
		next = fs.file.Header.NextUID
		return nil
	})
	return next, err
}
