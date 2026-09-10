package mailboxbase

import (
	"context"
	"io"
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// MessageSize is the size to report for a message: the record's own numbers
// when it has them, otherwise the driver's answer from storage (#1726).
func MessageSize(box mailbox.UserMailbox, folder string, m *mailbox.MessageMeta) (size, vsize uint32, err error) {
	if m == nil {
		return 0, 0, nil
	}
	if m.Size != 0 && m.VSize != 0 {
		return m.Size, m.VSize, nil
	}
	sizer, ok := mailbox.Driver(box).(mailbox.RecordSizer)
	if !ok {
		return m.Size, m.VSize, nil
	}
	return sizer.RecordSize(folder, m)
}

// RFC822SizeOf is MessageSize reduced to the one number RFC822.SIZE reports:
// the virtual size, falling back to the physical one the way mailbox.MessageMeta does.
func RFC822SizeOf(box mailbox.UserMailbox, folder string, m *mailbox.MessageMeta) uint32 {
	size, vsize, err := MessageSize(box, folder, m)
	if err != nil {
		return m.RFC822Size()
	}
	if vsize != 0 {
		return vsize
	}
	return size
}

// FillSizes stamps each record's sizes from storage where the record has none,
// so the callers that report a size read one answer, not two (#1726).
func FillSizes(box mailbox.UserMailbox, folder string, msgs []*mailbox.MessageMeta) {
	sizer, ok := mailbox.Driver(box).(mailbox.RecordSizer)
	if !ok {
		return
	}
	for _, m := range msgs {
		if m == nil || (m.Size != 0 && m.VSize != 0) {
			continue
		}
		size, vsize, err := sizer.RecordSize(folder, m)
		if err != nil {
			continue
		}
		m.Size, m.VSize = size, vsize
	}
}

// CountSizes reads a message and returns its physical and virtual sizes: the
// octet count on disk, and the one on the wire with every bare LF as CRLF.
func CountSizes(r io.Reader) (size, vsize uint32, err error) {
	buf := make([]byte, 32*1024)
	var prev byte
	for {
		n, rerr := r.Read(buf)
		for i := 0; i < n; i++ {
			size++
			vsize++
			if buf[i] == '\n' && prev != '\r' {
				vsize++
			}
			prev = buf[i]
		}
		if rerr == io.EOF {
			return size, vsize, nil
		}
		if rerr != nil {
			return 0, 0, rerr
		}
	}
}

// FillSizelessRecords gives the records that carry no size the one their driver
// holds, so the folder's sum stops reading them as empty (#1728).
func FillSizelessRecords(idx mailbox.UserIndex, box mailbox.UserMailbox, folder *mailbox.Folder) (int, error) {
	lister, canList := idx.(mailbox.SizelessLister)
	stamper, canStamp := idx.(mailbox.SizeStamper)
	if !canList || !canStamp {
		return 0, nil
	}
	if _, ok := mailbox.Driver(box).(mailbox.RecordSizer); !ok {
		return 0, nil
	}
	uids, err := lister.SizelessUIDs(folder.ID)
	if err != nil || len(uids) == 0 {
		return 0, err
	}
	msgs, err := ReadMessages(idx, folder.ID, mailbox.SeqSet{})
	if err != nil {
		return 0, err
	}
	want := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		want[uid] = struct{}{}
	}
	vsizes := make(map[uint32]uint32, len(uids))
	for _, m := range msgs {
		if _, missing := want[m.UID]; !missing {
			continue
		}
		_, vsize, serr := MessageSize(box, folder.Name, m)
		if serr != nil || vsize == 0 {
			continue
		}
		vsizes[m.UID] = vsize
	}
	if len(vsizes) == 0 {
		return 0, nil
	}
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		slog.Debug("mailbox: records carried no size; stamping what storage answers",
			"user", box.Username(), "folder", folder.Name, "sizeless", len(uids),
			"stamping", len(vsizes), "uids", uidsForDebug(vsizes))
	}
	return stamper.StampSizes(folder.ID, vsizes)
}

// uidsForDebug names the records a stamp touched, capped so one line stays one
// line on a folder that needs thousands.
func uidsForDebug(vsizes map[uint32]uint32) []uint32 {
	out := make([]uint32, 0, len(vsizes))
	for uid := range vsizes {
		if len(out) == 32 {
			break
		}
		out = append(out, uid)
	}
	return out
}

// ReadMessages is the read for a caller whose answer goes to a client and
// decides nothing on disk — FETCH, SEARCH, a JMAP query, a diagnostic dump. It
// takes the lock-free path where the index offers one.
//
// A caller whose answer chooses what to rewrite or delete must call
// GetMessages directly. That is not a preference: a stale answer there does not
// become visible a moment later, it decides wrongly and the write lands anyway.
// Keeping the two as separate calls is what makes the choice visible at the
// call site instead of hidden in an argument.
func ReadMessages(idx mailbox.UserIndex, folderID uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	if u, ok := idx.(mailbox.UnlockedReader); ok {
		return u.GetMessagesUnlocked(folderID, uids)
	}
	return idx.GetMessages(folderID, uids)
}
