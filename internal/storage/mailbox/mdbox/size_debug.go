package mdbox

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// sizeRowLookups counts the map lookups the size row itself makes. A row must
// cost nothing when it is not printed, and only a number proves that (#1740).
var sizeRowLookups atomic.Uint64

// SizeRowLookups is that count, for a test asserting the gate holds.
func SizeRowLookups() uint64 { return sizeRowLookups.Load() }

// ResetSizeRowLookups zeroes it.
func ResetSizeRowLookups() { sizeRowLookups.Store(0) }

// sizeDebug gates the readings the size row needs: the map lookup behind it is
// a second one, and only a debug run may pay for it (#1740).
func sizeDebug() bool {
	return slog.Default().Enabled(context.Background(), slog.LevelDebug)
}

// debugStorageSize names the frame a size was read from, so a wrong number
// reads as an offset or a neighbour's length instead of a guess.
func (u *userMailbox) debugStorageSize(folder string, uid, mapUID, size, vsize uint32) {
	if !sizeDebug() {
		return
	}
	attrs := []any{
		"user", u.username, "folder", folder, "uid", uid, "map_uid", mapUID,
		"size", size, "vsize", vsize,
	}
	sizeRowLookups.Add(1)
	if m, err := u.openMap(); err == nil {
		if e, ok, lerr := m.Lookup(mapUID); lerr == nil && ok {
			attrs = append(attrs, "file_id", e.FileID, "offset", e.Offset, "frame_size", e.Size)
		}
	}
	slog.Debug("mdbox: size read from storage, the record carried none", attrs...)
}
