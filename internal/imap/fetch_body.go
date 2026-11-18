package imap

import (
	"io"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// fetchBody opens one message once for every attribute of one FETCH response
// that reads it: an encrypted body pays its key unwrap on each open. A body
// with random access is shared as independent views and closed with the
// response; one without it, or an open that failed, opens again per caller,
// as before.
type fetchBody struct {
	s  *session
	m  *mailbox.MessageMeta
	rc io.ReadCloser
	rr mailbox.RangeReader
}

func (b *fetchBody) open() (io.ReadCloser, error) {
	if b.rr != nil {
		return bodyView{io.NewSectionReader(b.rr, 0, b.rr.Size())}, nil
	}
	rc, err := b.s.fetchSelected(b.m)
	if err != nil {
		return nil, err
	}
	rr, ok := rc.(mailbox.RangeReader)
	if !ok {
		return rc, nil
	}
	b.rc, b.rr = rc, rr
	return bodyView{io.NewSectionReader(rr, 0, rr.Size())}, nil
}

func (b *fetchBody) close() {
	if b.rc != nil {
		_ = b.rc.Close()
		b.rc, b.rr = nil, nil
	}
}

// bodyView is one reader over a shared body; closing it leaves the body open.
type bodyView struct{ *io.SectionReader }

func (bodyView) Close() error { return nil }
