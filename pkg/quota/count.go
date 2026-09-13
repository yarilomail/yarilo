package quota

import (
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// FolderVSizer is the slice of the index a count needs: a folder's aggregate
// virtual size. The folder is opened through the box (#1715).
type FolderVSizer interface {
	FolderVSize(folderID uint64) (bytes uint64, messages uint32, err error)
}

// CountUsage sums the index-derived aggregate virtual size and message count
// across the given folders — the authoritative quota usage (the count backend).
// Folders configured as "ignore" by limits are skipped so their messages do not
// count toward quota. Unreadable or absent folders are skipped rather than
// failing the whole read, mirroring how the aggregate self-heals: a transient
// per-folder error must not deny service on the user-wide total.
func CountUsage(box mailbox.Box, vs FolderVSizer, folders []string, limits Limits) Usage {
	var u Usage
	for _, name := range folders {
		if _, ignore := limits.EffectiveLimits(name); ignore {
			continue
		}
		MetricFoldersOpened.Inc()
		f, err := box.Folder(name, 0)
		if err != nil {
			continue
		}
		bytes, msgs, err := vs.FolderVSize(f.ID)
		if err != nil {
			continue
		}
		u.StorageBytes += int64(bytes)
		u.Messages += int64(msgs)
	}
	return u
}
