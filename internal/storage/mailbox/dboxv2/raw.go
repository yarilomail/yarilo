package dboxv2

import (
	"io"
	"os"
	"path/filepath"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// RawRecord is the body section of u.<uid> as stored: no decryption, no CRLF.
func (u *userMailbox) RawRecord(folder string, m *mailbox.MessageMeta) (mailbox.RawBody, error) {
	name, err := u.RecordPath(folder, m)
	if err != nil {
		return nil, err
	}
	f, sec, err := openBody(filepath.Join(u.folderPath(folder), name))
	if err != nil {
		return nil, err
	}
	return rawSection{SectionReader: sec, f: f}, nil
}

type rawSection struct {
	*io.SectionReader
	f *os.File
}

func (r rawSection) Close() error { return r.f.Close() }
