package mailbox

import (
	"fmt"
	"io"
)

// OpenMessage reads a message's body from what the record says. A driver that
// cannot answer from a record names no message here (#1700).
func OpenMessage(box UserMailbox, folder string, m *MessageMeta) (io.ReadCloser, error) {
	addr, ok := Driver(box).(UIDAddressable)
	if !ok {
		return nil, fmt.Errorf("mailbox: %T cannot find a message from its record", Driver(box))
	}
	return addr.OpenRecord(folder, m)
}

// MessagePath names a message for the operations that take a name -- copy,
// move, remove -- which stay the driver's own vocabulary.
func MessagePath(box UserMailbox, folder string, m *MessageMeta) (string, error) {
	addr, ok := Driver(box).(UIDAddressable)
	if !ok {
		return "", fmt.Errorf("mailbox: %T cannot name a message from its record", Driver(box))
	}
	return addr.RecordPath(folder, m)
}

// RemoveMessage unlinks it by the name the driver gives it.
func RemoveMessage(box UserMailbox, folder string, m *MessageMeta) error {
	name, err := MessagePath(box, folder, m)
	if err != nil {
		return err
	}
	return box.Remove(folder, name)
}

// Readable reports whether a message's body can be found from its record.
func Readable(box UserMailbox, m *MessageMeta) bool {
	_, ok := Driver(box).(UIDAddressable)
	return ok
}
