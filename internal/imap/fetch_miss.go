package imap

import (
	"errors"
	"fmt"
	"io"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// errNoRecordAddress is a FETCH item the store cannot find from the record.
var errNoRecordAddress = errors.New("imap: the store cannot find this message from its record")

// missReader records what a parse over a message body cannot report itself:
// how much it read and the first failure that was not the end of the file.
type missReader struct {
	r   io.Reader
	n   int64
	err error
}

func (mr *missReader) Read(p []byte) (int, error) {
	n, err := mr.r.Read(p)
	mr.n += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && mr.err == nil {
		mr.err = err
	}
	return n, err
}

// missed is the read error behind a parse, or an empty read of a message its
// record says is not empty; nil when the parse saw the message.
func (mr *missReader) missed(m *mailbox.MessageMeta) error {
	if mr.err != nil {
		return mr.err
	}
	if mr.n == 0 && m.Size > 0 {
		return fmt.Errorf("imap: message file read empty, its record says %d bytes", m.Size)
	}
	return nil
}
