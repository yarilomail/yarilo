package virtual

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// IndexFileName holds the virtual mailbox's own uid space.
const IndexFileName = "yarilo-virtual.index"

const (
	indexMagic   = "YVIRT"
	indexVersion = 1
	headerSize   = 5 + 1 + 4 + 4 + 4 + 4  // magic, version, and the four header words
	boxRecSize   = 16 + 4 + 4 + 4 + 8 + 4 // guid, id, uidvalidity, nextuid, modseq, name length
	msgRecSize   = 4 + 4
)

// Header is the reference's virtual index header (virtual-storage.h:20-45),
// with one change: a backing folder is named by its GUID, so a rename cannot
// break the mapping (#1995).
type Header struct {
	// ChangeCounter rises with every write of the header.
	ChangeCounter uint32
	// HighestMailboxID is never reused, so a stale record cannot be read as
	// belonging to a folder that took its number afterwards.
	HighestMailboxID uint32
	// SearchArgsCRC32 is the configuration's rules: a different value means
	// the set is defined differently and the mailbox is rebuilt.
	SearchArgsCRC32 uint32
}

// BackingBox is one backing folder as the index remembers it.
type BackingBox struct {
	ID            uint32
	GUID          [16]byte
	Name          string
	UIDValidity   uint32
	NextUID       uint32
	HighestModSeq uint64
}

// Record maps one virtual uid (the position in the slice) to a real message.
type Record struct {
	MailboxID uint32
	RealUID   uint32
}

// Index is the whole file: what the mailbox was made of, and what it held.
type Index struct {
	Header  Header
	Boxes   []BackingBox
	Records []Record
}

// BoxByGUID finds a backing folder by its identity.
func (ix *Index) BoxByGUID(guid [16]byte) (BackingBox, bool) {
	for _, b := range ix.Boxes {
		if b.GUID == guid {
			return b, true
		}
	}
	return BackingBox{}, false
}

// Assign returns the id this folder already has, or the next one. Ids are
// never reused, so a record written before a folder left still names it.
func (ix *Index) Assign(guid [16]byte, name string, uidValidity uint32) uint32 {
	for i := range ix.Boxes {
		if ix.Boxes[i].GUID == guid {
			ix.Boxes[i].Name = name
			ix.Boxes[i].UIDValidity = uidValidity
			return ix.Boxes[i].ID
		}
	}
	ix.Header.HighestMailboxID++
	ix.Boxes = append(ix.Boxes, BackingBox{
		ID: ix.Header.HighestMailboxID, GUID: guid, Name: name, UIDValidity: uidValidity,
	})
	return ix.Header.HighestMailboxID
}

// NeedsRebuild says the configuration no longer describes what was indexed.
func (ix *Index) NeedsRebuild(crc uint32) bool { return ix.Header.SearchArgsCRC32 != crc }

// ErrNoIndex says the mailbox has no index yet.
var ErrNoIndex = errors.New("virtual: no index")

// LoadIndex reads the index of the virtual mailbox in dir.
func LoadIndex(dir string) (*Index, error) {
	raw, err := os.ReadFile(filepath.Join(dir, IndexFileName))
	if os.IsNotExist(err) {
		return nil, ErrNoIndex
	}
	if err != nil {
		return nil, fmt.Errorf("virtual: read index: %w", err)
	}
	return decodeIndex(raw)
}

// Save writes the index whole: the file is small and derived, so a torn write
// is repaired by rebuilding rather than by a journal.
func (ix *Index) Save(dir string) error {
	tmp := filepath.Join(dir, IndexFileName+".tmp")
	if err := os.WriteFile(tmp, ix.encode(), 0o600); err != nil {
		return fmt.Errorf("virtual: write index: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, IndexFileName)); err != nil {
		return fmt.Errorf("virtual: replace index: %w", err)
	}
	return nil
}

func (ix *Index) encode() []byte {
	ix.Header.ChangeCounter++
	out := make([]byte, 0, headerSize+len(ix.Boxes)*boxRecSize+len(ix.Records)*msgRecSize)
	out = append(out, indexMagic...)
	out = append(out, indexVersion)
	out = binary.LittleEndian.AppendUint32(out, ix.Header.ChangeCounter)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(ix.Boxes)))
	out = binary.LittleEndian.AppendUint32(out, ix.Header.HighestMailboxID)
	out = binary.LittleEndian.AppendUint32(out, ix.Header.SearchArgsCRC32)
	for _, b := range ix.Boxes {
		out = append(out, b.GUID[:]...)
		out = binary.LittleEndian.AppendUint32(out, b.ID)
		out = binary.LittleEndian.AppendUint32(out, b.UIDValidity)
		out = binary.LittleEndian.AppendUint32(out, b.NextUID)
		out = binary.LittleEndian.AppendUint64(out, b.HighestModSeq)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(b.Name)))
	}
	for _, b := range ix.Boxes {
		out = append(out, b.Name...)
	}
	for _, r := range ix.Records {
		out = binary.LittleEndian.AppendUint32(out, r.MailboxID)
		out = binary.LittleEndian.AppendUint32(out, r.RealUID)
	}
	return out
}

func decodeIndex(raw []byte) (*Index, error) {
	if len(raw) < headerSize || string(raw[:len(indexMagic)]) != indexMagic {
		return nil, errors.New("virtual: the index is not one of ours")
	}
	if raw[len(indexMagic)] != indexVersion {
		return nil, fmt.Errorf("virtual: index version %d is not %d", raw[len(indexMagic)], indexVersion)
	}
	ix := &Index{}
	p := len(indexMagic) + 1
	ix.Header.ChangeCounter = binary.LittleEndian.Uint32(raw[p:])
	boxCount := binary.LittleEndian.Uint32(raw[p+4:])
	ix.Header.HighestMailboxID = binary.LittleEndian.Uint32(raw[p+8:])
	ix.Header.SearchArgsCRC32 = binary.LittleEndian.Uint32(raw[p+12:])
	p += 16
	if int(boxCount)*boxRecSize > len(raw)-p {
		return nil, errors.New("virtual: the index is shorter than its own count of folders")
	}
	nameLens := make([]uint32, boxCount)
	for i := uint32(0); i < boxCount; i++ {
		var b BackingBox
		copy(b.GUID[:], raw[p:p+16])
		b.ID = binary.LittleEndian.Uint32(raw[p+16:])
		b.UIDValidity = binary.LittleEndian.Uint32(raw[p+20:])
		b.NextUID = binary.LittleEndian.Uint32(raw[p+24:])
		b.HighestModSeq = binary.LittleEndian.Uint64(raw[p+28:])
		nameLens[i] = binary.LittleEndian.Uint32(raw[p+36:])
		ix.Boxes = append(ix.Boxes, b)
		p += boxRecSize
	}
	for i := range ix.Boxes {
		n := int(nameLens[i])
		if p+n > len(raw) {
			return nil, errors.New("virtual: a folder name runs past the end of the index")
		}
		ix.Boxes[i].Name = string(raw[p : p+n])
		p += n
	}
	for p+msgRecSize <= len(raw) {
		ix.Records = append(ix.Records, Record{
			MailboxID: binary.LittleEndian.Uint32(raw[p:]),
			RealUID:   binary.LittleEndian.Uint32(raw[p+4:]),
		})
		p += msgRecSize
	}
	return ix, nil
}
