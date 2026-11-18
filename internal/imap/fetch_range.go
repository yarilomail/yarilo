package imap

import (
	"errors"
	"io"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// wholeMessageRange is BODY[]<n.m>: a byte range of the whole message, which
// a RangeReader answers without reading the bytes before it.
func wholeMessageRange(s *imaplib.FetchItemBodySection) bool {
	return s.Partial != nil && s.Specifier == imaplib.PartSpecifierNone && len(s.Part) == 0 &&
		len(s.HeaderFields) == 0 && len(s.HeaderFieldsNot) == 0
}

// readRange answers as the library's partial extraction does: nil past the
// end, the range clamped to what the message holds.
func readRange(r mailbox.RangeReader, p *imaplib.SectionPartial) []byte {
	total := r.Size()
	if p.Offset > total {
		return nil
	}
	n := total - p.Offset
	if p.Size >= 0 && p.Size < n {
		n = p.Size
	}
	buf := make([]byte, n)
	got, err := r.ReadAt(buf, p.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return buf[:got]
}

// readStreamRange is readRange over a body with no random access: the bytes
// before the range are read and dropped, nothing is parsed, so the range of a
// body that is not a parsable message is still the range (#2581).
func readStreamRange(r io.Reader, p *imaplib.SectionPartial) []byte {
	skipped, err := io.CopyN(io.Discard, r, p.Offset)
	if skipped < p.Offset {
		return nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	src := r
	if p.Size >= 0 {
		src = io.LimitReader(r, p.Size)
	}
	out, err := io.ReadAll(src)
	if err != nil {
		return nil
	}
	return out
}
