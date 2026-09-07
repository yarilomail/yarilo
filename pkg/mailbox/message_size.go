package mailbox

import "io"

// RecordSizer answers a message's sizes from where its driver keeps them. A
// maildir name carries both; a dbox record already holds them (#1726).
type RecordSizer interface {
	RecordSize(folder string, m *MessageMeta) (size, vsize uint32, err error)
}

// MessageSize is the size to report for a message: the record's own numbers
// when it has them, otherwise the driver's answer from storage (#1726).
func MessageSize(box UserMailbox, folder string, m *MessageMeta) (size, vsize uint32, err error) {
	if m == nil {
		return 0, 0, nil
	}
	if m.Size != 0 && m.VSize != 0 {
		return m.Size, m.VSize, nil
	}
	sizer, ok := Driver(box).(RecordSizer)
	if !ok {
		return m.Size, m.VSize, nil
	}
	return sizer.RecordSize(folder, m)
}

// RFC822SizeOf is MessageSize reduced to the one number RFC822.SIZE reports:
// the virtual size, falling back to the physical one the way MessageMeta does.
func RFC822SizeOf(box UserMailbox, folder string, m *MessageMeta) uint32 {
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
func FillSizes(box UserMailbox, folder string, msgs []*MessageMeta) {
	sizer, ok := Driver(box).(RecordSizer)
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
func FillSizelessRecords(idx UserIndex, box UserMailbox, folder *Folder) (int, error) {
	lister, canList := idx.(SizelessLister)
	stamper, canStamp := idx.(SizeStamper)
	if !canList || !canStamp {
		return 0, nil
	}
	if _, ok := Driver(box).(RecordSizer); !ok {
		return 0, nil
	}
	uids, err := lister.SizelessUIDs(folder.ID)
	if err != nil || len(uids) == 0 {
		return 0, err
	}
	msgs, err := ReadMessages(idx, folder.ID, SeqSet{})
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
	return stamper.StampSizes(folder.ID, vsizes)
}
