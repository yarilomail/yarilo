package mdbox

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// RawRecord is a record's body inside its m.<N> as stored: no decryption, no
// CRLF. A purge that moved the record is followed once, as Fetch does.
func (u *userMailbox) RawRecord(folder string, m *mailbox.MessageMeta) (mailbox.RawBody, error) {
	if _, err := u.RecordPath(folder, m); err != nil {
		return nil, err
	}
	mp, err := u.openMap()
	if err != nil {
		return nil, err
	}
	entry, ok, err := mp.Lookup(m.MapUID)
	if err != nil {
		return nil, fmt.Errorf("mdbox/raw: lookup: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("mdbox/raw: map_uid %d: %w", m.MapUID, mailbox.ErrExpunged)
	}
	b, err := u.rawEntry(entry, m.AltTier)
	if !errors.Is(err, os.ErrNotExist) {
		return b, err
	}
	fresh, ok, err := mp.LookupReloaded(m.MapUID)
	if err != nil {
		return nil, fmt.Errorf("mdbox/raw: reload: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("mdbox/raw: map_uid %d: %w", m.MapUID, mailbox.ErrExpunged)
	}
	return u.rawEntry(fresh, m.AltTier)
}

func (u *userMailbox) rawEntry(entry mdboxmap.MapEntry, altTier bool) (mailbox.RawBody, error) {
	f, err := u.openEntryFile(entry.FileID, altTier)
	if err != nil {
		return nil, err
	}
	bodyOff, size, err := readRecordHeader(f, entry.Offset)
	if err != nil {
		_ = f.Close()
		return nil, corruptFetchErr(entry.FileID, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mdbox/raw: stat m.%d: %w", entry.FileID, err)
	}
	if bodyOff+int64(size) > st.Size() {
		_ = f.Close()
		return nil, corruptFetchErr(entry.FileID, io.ErrUnexpectedEOF)
	}
	return rawSection{SectionReader: io.NewSectionReader(f, bodyOff, int64(size)), f: f}, nil
}

// openEntryFile opens m.<id> in the tier the index names, falling back to the
// other one as openEntry does.
func (u *userMailbox) openEntryFile(fileID uint32, altTier bool) (*os.File, error) {
	if altTier && u.AltEnabled() {
		if f, err := os.Open(u.mfileAltPath(fileID)); err == nil {
			return f, nil
		}
	}
	f, err := os.Open(u.mfilePath(fileID))
	if errors.Is(err, os.ErrNotExist) && u.AltEnabled() {
		f, err = os.Open(u.mfileAltPath(fileID))
	}
	if err != nil {
		return nil, fmt.Errorf("mdbox/raw: open m.%d: %w", fileID, err)
	}
	return f, nil
}

type rawSection struct {
	*io.SectionReader
	f *os.File
}

func (r rawSection) Close() error { return r.f.Close() }

// RawUnreferenced calls fn with every map record no folder refers to: expunged,
// not yet purged, and still mail a restore brings back.
func (u *userMailbox) RawUnreferenced(fn func(mapUID uint32, b mailbox.RawBody) error) error {
	mp, err := u.openMap()
	if err != nil {
		return err
	}
	recs, err := mp.RecordsReloaded()
	if err != nil {
		return fmt.Errorf("mdbox/raw: records: %w", err)
	}
	for _, e := range recs {
		if e.RefCount != 0 {
			continue
		}
		b, err := u.rawEntry(e, false)
		if err != nil {
			return fmt.Errorf("mdbox/raw: map_uid %d: %w", e.UID, err)
		}
		err = fn(e.UID, b)
		_ = b.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
