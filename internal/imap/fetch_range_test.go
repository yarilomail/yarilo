package imap

import (
	"bytes"
	"crypto/rand"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

type countingRange struct {
	*bytes.Reader
	read int64
}

func (c *countingRange) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.Reader.ReadAt(p, off)
	c.read += int64(n)
	return n, err
}

// TestRangeAnswersAsTheLibrary: BODY[]<n.m> from a RangeReader gives the
// bytes the library's extraction gives, reading only the range.
func TestRangeAnswersAsTheLibrary(t *testing.T) {
	body := make([]byte, 100000)
	_, _ = rand.Read(body)
	msg := append([]byte("From: a@b\r\nSubject: s\r\n\r\n"), body...)
	cases := []imaplib.SectionPartial{
		{Offset: 0, Size: 10}, {Offset: 50000, Size: 4096}, {Offset: int64(len(msg)) - 5, Size: 100},
		{Offset: int64(len(msg)), Size: 10}, {Offset: int64(len(msg)) + 1, Size: 10}, {Offset: 7, Size: 0},
	}
	for _, p := range cases {
		section := &imaplib.FetchItemBodySection{Partial: &p}
		if !wholeMessageRange(section) {
			t.Fatal("BODY[]<n.m> not recognised")
		}
		want := imapserver.ExtractBodySection(bytes.NewReader(msg), section)
		rr := &countingRange{Reader: bytes.NewReader(msg)}
		got := readRange(rr, &p)
		if !bytes.Equal(got, want) || (got == nil) != (want == nil) {
			t.Fatalf("<%d.%d>: %d bytes (nil %v), library %d (nil %v)", p.Offset, p.Size, len(got), got == nil, len(want), want == nil)
		}
		if rr.read > p.Size {
			t.Fatalf("<%d.%d>: read %d bytes", p.Offset, p.Size, rr.read)
		}
	}
	for _, s := range []*imaplib.FetchItemBodySection{
		{Partial: &cases[0], Part: []int{1}},
		{Partial: &cases[0], Specifier: imaplib.PartSpecifierHeader},
		{},
	} {
		if wholeMessageRange(s) {
			t.Fatalf("%+v taken for a whole-message range", s)
		}
	}
}
