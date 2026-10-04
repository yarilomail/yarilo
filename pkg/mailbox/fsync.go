package mailbox

import "fmt"

// FsyncMode says what reaches the disk before a delivery is acknowledged. The
// reference's three, with its default (#1847).
type FsyncMode string

const (
	// FsyncNever syncs nothing, for a volume whose durability is somebody
	// else's -- a stand, or storage that acknowledges its own writes.
	FsyncNever FsyncMode = "never"
	// FsyncOptimized syncs the message body before the delivery is answered.
	FsyncOptimized FsyncMode = "optimized"
	// FsyncAlways adds the index, the list and the directory entry.
	FsyncAlways FsyncMode = "always"
)

// ParseFsyncMode reads the configured mode, defaulting to optimized.
func ParseFsyncMode(s string) (FsyncMode, error) {
	switch FsyncMode(s) {
	case "", FsyncOptimized:
		return FsyncOptimized, nil
	case FsyncNever:
		return FsyncNever, nil
	case FsyncAlways:
		return FsyncAlways, nil
	}
	return "", fmt.Errorf("mailbox: unknown mail_fsync %q (want never, optimized or always)", s)
}

// SyncsBody answers for the body a delivery has just written.
func (m FsyncMode) SyncsBody() bool { return m != FsyncNever }

// SyncsList answers for the uidlist row: it names the message, and a lost row
// hands the same file a second uid, so optimized keeps it.
func (m FsyncMode) SyncsList() bool { return m != FsyncNever }

// SyncsIndex answers for the index and its journal, which a crash rebuilds
// from the bodies and the list.
func (m FsyncMode) SyncsIndex() bool { return m == FsyncAlways }

// SyncsDir answers for the directory entry after a rename or a create.
func (m FsyncMode) SyncsDir() bool { return m == FsyncAlways }
