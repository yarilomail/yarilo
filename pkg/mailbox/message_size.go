package mailbox

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
