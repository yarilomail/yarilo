package file

import (
	"encoding/binary"
	"fmt"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The virtual extension: a virtual mailbox's record says which backing folder
// a message came from and its uid there, and the header says what the set was
// made of (virtual-storage.h:20-45, 62-66).
const (
	extNameVirtual = "virtual"
	virtualRecSize = 8
	virtualHdrHead = 16 // change counter, highest id, crc32, backing count
	virtualBoxSize = 36 // guid, id, uidvalidity, nextuid, modseq, name length
)

func encodeVirtualRec(backing, realUID uint32) []byte {
	b := make([]byte, virtualRecSize)
	binary.LittleEndian.PutUint32(b[0:4], backing)
	binary.LittleEndian.PutUint32(b[4:8], realUID)
	return b
}

// decodeVirtualRec reads it; a short or absent field is "this record does not
// say", which is every record written by a folder that is not virtual.
func decodeVirtualRec(b []byte) (backing, realUID uint32) {
	if len(b) < virtualRecSize {
		return 0, 0
	}
	return binary.LittleEndian.Uint32(b[0:4]), binary.LittleEndian.Uint32(b[4:8])
}

func encodeVirtualHdr(h mailbox.VirtualHeader) []byte {
	h.ChangeCounter++
	out := make([]byte, 0, virtualHdrHead+len(h.Backing)*virtualBoxSize)
	out = binary.LittleEndian.AppendUint32(out, h.ChangeCounter)
	out = binary.LittleEndian.AppendUint32(out, h.HighestBackingID)
	out = binary.LittleEndian.AppendUint32(out, h.SearchCRC32)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(h.Backing)))
	for _, b := range h.Backing {
		out = append(out, b.GUID[:]...)
		out = binary.LittleEndian.AppendUint32(out, b.ID)
		out = binary.LittleEndian.AppendUint32(out, b.UIDValidity)
		out = binary.LittleEndian.AppendUint32(out, b.NextUID)
		out = binary.LittleEndian.AppendUint64(out, b.HighestModSeq)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(b.Name)))
	}
	for _, b := range h.Backing {
		out = append(out, b.Name...)
	}
	return out
}

func decodeVirtualHdr(raw []byte) (mailbox.VirtualHeader, bool) {
	if len(raw) < virtualHdrHead {
		return mailbox.VirtualHeader{}, false
	}
	h := mailbox.VirtualHeader{
		ChangeCounter:    binary.LittleEndian.Uint32(raw[0:]),
		HighestBackingID: binary.LittleEndian.Uint32(raw[4:]),
		SearchCRC32:      binary.LittleEndian.Uint32(raw[8:]),
	}
	count := binary.LittleEndian.Uint32(raw[12:])
	p := virtualHdrHead
	if int(count)*virtualBoxSize > len(raw)-p {
		return mailbox.VirtualHeader{}, false
	}
	nameLens := make([]uint32, count)
	for i := uint32(0); i < count; i++ {
		var b mailbox.VirtualBacking
		copy(b.GUID[:], raw[p:p+16])
		b.ID = binary.LittleEndian.Uint32(raw[p+16:])
		b.UIDValidity = binary.LittleEndian.Uint32(raw[p+20:])
		b.NextUID = binary.LittleEndian.Uint32(raw[p+24:])
		b.HighestModSeq = binary.LittleEndian.Uint64(raw[p+28:])
		nameLens[i] = binary.LittleEndian.Uint32(raw[p+32:])
		h.Backing = append(h.Backing, b)
		p += virtualBoxSize
	}
	for i := range h.Backing {
		n := int(nameLens[i])
		if p+n > len(raw) {
			return mailbox.VirtualHeader{}, false
		}
		h.Backing[i].Name = string(raw[p : p+n])
		p += n
	}
	return h, true
}

// VirtualHeader reads what this folder's index says about the set it holds.
// Absent reads as "not a virtual mailbox", which every other folder is.
func (u *userIndex) VirtualHeader(folderID uint64) (mailbox.VirtualHeader, bool) {
	var (
		out mailbox.VirtualHeader
		ok  bool
	)
	if err := u.withFolderROUnlocked(folderID, func(fs *folderState) error {
		if ext := findExt(fs.file.Extensions, extNameVirtual); ext != nil {
			out, ok = decodeVirtualHdr(ext.HdrData)
		}
		return nil
	}); err != nil {
		return mailbox.VirtualHeader{}, false
	}
	return out, ok
}

// SetVirtualHeader records it, declaring the extension on first use so that a
// record can name its backing folder.
func (u *userIndex) SetVirtualHeader(folderID uint64, h mailbox.VirtualHeader) error {
	return u.withFolderSite(folderID, lockSiteVirtualHeader, func(fs *folderState) error {
		data := encodeVirtualHdr(h)
		if ext := findExt(fs.file.Extensions, extNameVirtual); ext != nil {
			ext.HdrData, ext.HdrSize = data, uint32(len(data))
			return fs.flush()
		}
		if err := fs.declareRecordExtLocked(extNameVirtual, data, virtualRecSize, 4, 0); err != nil {
			return fmt.Errorf("fileindex/virtual: %w", err)
		}
		return fs.flush()
	})
}
