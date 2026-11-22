package mailindex

import (
	"encoding/binary"
	"fmt"
)

// RecordMinSize is the on-disk size of the base record (uid + flags)
// before any extension bytes are appended. Equals sizeof(uint32) +
// sizeof(uint8).
const RecordMinSize = 5

// MailFlag bits live in a record's flags byte.
type MailFlag uint8

// IMAP mail-flags subset stored in the base record. Order/values
// match the on-disk encoding the canonical reader expects.
const (
	FlagAnswered MailFlag = 0x01
	FlagFlagged  MailFlag = 0x02
	FlagDeleted  MailFlag = 0x04
	FlagSeen     MailFlag = 0x08
	FlagDraft    MailFlag = 0x10
	// FlagUnused (0x20) is reserved and always zero on write -
	// pre-format-7 used it for MAIL_RECENT.
	FlagUnused MailFlag = 0x20
	// FlagBackend (0x40) is a per-driver scratch bit. The
	// mail-index layer does not interpret it.
	FlagBackend MailFlag = 0x40
	// FlagDirty (0x80) marks a record whose flag bits have not
	// yet been flushed to the storage backend.
	FlagDirty MailFlag = 0x80
)

// FlagsMask is the set of flags the mail-index treats as
// IMAP-visible (matches MAIL_INDEX_FLAGS_MASK). Backend/dirty
// bits are NOT in this mask - they're per-driver internal state.
const FlagsMask MailFlag = FlagAnswered | FlagFlagged | FlagDeleted | FlagSeen | FlagDraft

// NewRecord is a zeroed record of the layout carrying uid and flags; an
// extension's bytes are zero until SetExt writes them, as for a record that
// predates the extension.
func (l RecordLayout) NewRecord(uid uint32, flags MailFlag) []byte {
	rec := make([]byte, l.RecordSize)
	binary.LittleEndian.PutUint32(rec[0:], uid)
	rec[4] = uint8(flags)
	return rec
}

// RecordUID and RecordFlags read the base fields of a record in any layout.
func RecordUID(rec []byte) uint32     { return binary.LittleEndian.Uint32(rec[0:]) }
func RecordFlags(rec []byte) MailFlag { return MailFlag(rec[4]) }

// ExtOf is the named extension's bytes in rec, a view into it; nil when the
// layout carries no record bytes for that name.
func (l RecordLayout) ExtOf(rec []byte, name string) []byte {
	id, ok := l.ExtID(name)
	if !ok {
		return nil
	}
	e := l.Extensions[id]
	return rec[e.RecordOffset : e.RecordOffset+e.RecordSize]
}

// SetExt writes data as the named extension's bytes in rec; data must be the
// extension's record size.
func (l RecordLayout) SetExt(rec []byte, name string, data []byte) error {
	if uint32(len(rec)) != l.RecordSize {
		return fmt.Errorf("mailindex: set ext: record %d bytes, layout %d: %w", len(rec), l.RecordSize, ErrCorrupted)
	}
	id, ok := l.ExtID(name)
	if !ok {
		return fmt.Errorf("mailindex: set ext: layout carries no record bytes for %q: %w", name, ErrCorrupted)
	}
	e := l.Extensions[id]
	if uint16(len(data)) != e.RecordSize {
		return fmt.Errorf("mailindex: set ext: %q has %d bytes, want %d: %w", name, len(data), e.RecordSize, ErrCorrupted)
	}
	copy(rec[e.RecordOffset:], data)
	return nil
}
