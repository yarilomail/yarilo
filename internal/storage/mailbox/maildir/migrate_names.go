package maildir

import (
	"os"
	"path/filepath"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// MigrateUIDNames takes nothing from an older build's sidecar and removes it:
// on maildir the list is the mapping (#1700).
func (u *userMailbox) MigrateUIDNames(idx mailbox.UserIndex, folder *mailbox.Folder) (int, error) {
	forgetter, ok := idx.(mailbox.StoredNameForgetter)
	if !ok {
		return 0, nil
	}
	if !u.sidecarPresent(idx, folder) {
		return 0, nil
	}
	return 0, forgetter.ForgetStoredNames(folder.ID)
}

// sidecarPresent is one stat, so an open costs nothing once the file is gone.
func (u *userMailbox) sidecarPresent(idx mailbox.UserIndex, folder *mailbox.Folder) bool {
	dir, ok := idx.(interface{ IndexDirFor(string) string })
	if !ok {
		return false
	}
	_, err := os.Stat(filepath.Join(dir.IndexDirFor(folder.Name), indexNamesFileName))
	return err == nil
}

// indexNamesFileName is the sidecar's name, spelled here so this driver can ask
// whether one is there without reaching into the index package.
const indexNamesFileName = "yarilo.index.names"
