package dboxv2

import (
	"encoding/hex"
	"strings"
	"time"

	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// MigrateUIDNames renames what earlier builds stored under a GUID name to
// u.<uid>. Once per folder: the marker answers for a migrated one (#1704).
func (u *userMailbox) MigrateUIDNames(idx mailbox.UserIndex, folder *mailbox.Folder) (int, error) {
	// The fact belongs to the folder's index, not to a file among the mail.
	marker, ok := idx.(mailbox.UIDNameMarker)
	if !ok {
		return 0, fmt.Errorf("sdbox/migrate: %q: the index cannot record the pass", folder.Name)
	}
	switch done, err := marker.UIDNamed(folder.ID); {
	case err != nil:
		return 0, fmt.Errorf("sdbox/migrate: read marker %q: %w", folder.Name, err)
	case done:
		return 0, nil
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		return 0, fmt.Errorf("sdbox/migrate: get messages %q: %w", folder.Name, err)
	}
	renamed := make(map[uint32]string, len(msgs))
	leftovers, placed := 0, 0
	err = u.withMailboxLock(folder.Name, func() error {
		dir := u.folderPath(folder.Name)
		if serr := sweepStaleTemps(dir); serr != nil {
			return serr
		}
		for _, m := range msgs {
			want := sdboxMailPrefix + strconv.FormatUint(uint64(m.UID), 10)
			// A record that lost its name still names its file: the old one was
			// the GUID in hex, and the GUID is in the record (#1713).
			old := sdboxMailPrefix + hex.EncodeToString(m.GUID[:])
			if old == want {
				continue
			}
			if _, serr := os.Lstat(filepath.Join(dir, want)); serr == nil {
				// The record is already named: renaming a guid-named twin over
				// the file it reads would swap the message for it (#1718).
				continue
			}
			from := filepath.Join(dir, old)
			if _, serr := os.Lstat(from); serr != nil {
				// Nothing to rename: a record whose file is already gone is the
				// reactive heal's business, not this pass's.
				continue
			}
			if err := os.Rename(from, filepath.Join(dir, want)); err != nil {
				return fmt.Errorf("sdbox/migrate: rename %s -> %s: %w", old, want, err)
			}
			renamed[m.UID] = want
		}
		adopted, left, cerr := u.adoptOrphans(idx, folder, msgs)
		leftovers = left
		placed = adopted
		return cerr
	})
	if err != nil {
		return 0, err
	}
	if leftovers > 0 {
		// Not marked: what is left is a save still in flight, and its own caller
		// names it within the cycle -- the next pass finds it placed (#1713).
		slog.Warn("sdbox: the folder still holds a message named by a guid",
			"user", u.username, "folder", folder.Name, "left", leftovers, "adopted", placed)
		return len(renamed) + placed, nil
	}
	if len(renamed)+placed == 0 {
		return 0, marker.MarkUIDNamed(folder.ID)
	}
	// After the rename, never before: a crash between the two leaves a file the
	// index cannot name, and the next pass renames nothing and repairs it.
	// Marked last: a crash before it repeats a pass that renames nothing. The
	// names are not recorded at all -- u.<uid> is the record's own answer.
	if err := marker.MarkUIDNamed(folder.ID); err != nil {
		return 0, err
	}
	slog.Info("sdbox: renamed messages to the name their uid gives them",
		"user", u.username, "folder", folder.Name, "renamed", len(renamed), "adopted", placed)
	return len(renamed) + placed, nil
}

// staleTemp is the shared rule: a save names its file within a cycle, so
// anything this old is a crash's leftover (#1736).
const staleTemp = mailbox.StaleTemp

// sweepStaleTemps removes those leftovers. A young one is left alone: it is a
// save in flight, and its own caller is about to name it.
func sweepStaleTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sdbox/migrate: list %s: %w", dir, err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), temporaryPrefix) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || time.Since(info.ModTime()) < staleTemp {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("sdbox/migrate: remove %s: %w", e.Name(), rerr)
		}
		slog.Info("sdbox: removed a save that never got a uid", "file", e.Name(), "dir", dir)
	}
	return nil
}
