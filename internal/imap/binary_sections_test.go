package imap_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

const binaryMultipart = "From: a@b\r\n" +
	"Subject: parts\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=XX\r\n" +
	"\r\n" +
	"--XX\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"caf=C3=A9\r\n" +
	"--XX\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"AAEC\r\n" +
	"--XX--\r\n"

const binarySingle = "From: a@b\r\n" +
	"Subject: needle here\r\n" +
	"\r\n" +
	"body\r\n"

// Each section is its own bytes decoded: the whole message for [], the body of
// a single-part message for [1], and the part named in a multipart one.
func TestFetchBinarySections(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		part []int
		want string
	}{
		{"whole single-part message", binarySingle, nil, binarySingle},
		{"part 1 of a single-part message is its body", binarySingle, []int{1}, "body\r\n"},
		{"multipart, whole message unchanged", binaryMultipart, nil, binaryMultipart},
		{"multipart, quoted-printable part", binaryMultipart, []int{1}, "café"},
		{"multipart, base64 part with NULs", binaryMultipart, []int{2}, "\x00\x01\x02"},
		{"a part that does not exist is empty", binaryMultipart, []int{3}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := startAuthClient(t, "user@test.com", "testpass")
			defer func() { c.Logout().Wait() }() //nolint:errcheck
			appendWithFlags(t, c, "INBOX", []byte(tc.raw))
			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			seq := imap.SeqSetNum(1)
			msgs, err := c.Fetch(seq, &imap.FetchOptions{
				BinarySection:     []*imap.FetchItemBinarySection{{Part: tc.part, Peek: true}},
				BinarySectionSize: []*imap.FetchItemBinarySectionSize{{Part: tc.part}},
			}).Collect()
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 || len(msgs[0].BinarySection) != 1 || len(msgs[0].BinarySectionSize) != 1 {
				t.Fatalf("FETCH answered %+v", msgs)
			}
			if got := string(msgs[0].BinarySection[0].Bytes); got != tc.want {
				t.Errorf("BINARY%v = %q, want %q", tc.part, got, tc.want)
			}
			if got := msgs[0].BinarySectionSize[0].Size; got != uint32(len(tc.want)) {
				t.Errorf("BINARY.SIZE%v = %d, want %d: the size is of the same bytes", tc.part, got, len(tc.want))
			}
		})
	}
}

// A part in an encoding the server cannot undo fails the command with
// UNKNOWN-CTE (RFC 3516 4.3), rather than handing the client encoded bytes.
func TestFetchBinaryUnknownEncodingIsRefused(t *testing.T) {
	raw := strings.Replace(binaryMultipart, "Content-Transfer-Encoding: base64", "Content-Transfer-Encoding: x-uuencode", 1)
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	appendWithFlags(t, c, "INBOX", []byte(raw))
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	_, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BinarySection: []*imap.FetchItemBinarySection{{Part: []int{2}, Peek: true}},
	}).Collect()
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeUnknownCTE {
		t.Errorf("FETCH answered %v, want NO [UNKNOWN-CTE]", err)
	}
}
