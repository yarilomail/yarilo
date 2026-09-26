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

// binaryMultipartDecoded is binaryMultipart as BINARY[] answers it: each leaf
// decoded, its header relabelled binary, everything else byte for byte.
const binaryMultipartDecoded = "From: a@b\r\n" +
	"Subject: parts\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=XX\r\n" +
	"\r\n" +
	"--XX\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: binary\r\n" +
	"\r\n" +
	"café\r\n" +
	"--XX\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Transfer-Encoding: binary\r\n" +
	"\r\n" +
	"\x00\x01\x02\r\n" +
	"--XX--\r\n"

const binaryBase64Single = "From: a@b\r\n" +
	"Content-Transfer-Encoding:\r\n" +
	" base64\r\n" +
	"Subject: folded\r\n" +
	"\r\n" +
	"aGVsbG8=\r\n"

const binaryNested = "From: a@b\r\n" +
	"Content-Type: message/rfc822\r\n" +
	"\r\n" +
	"From: c@d\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"aGVsbG8=\r\n"

func qpMessage(body string) string {
	return "From: a@b\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" + body
}

// A folded field after the CTE one must not join its value: "base64 b" is an
// encoding nobody knows.
const binaryFoldedAfter = "Content-Transfer-Encoding: base64\r\n" +
	"Subject: a\r\n" +
	" b\r\n" +
	"\r\n" +
	"aGVsbG8=\r\n"

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
		{"multipart, every leaf decoded", binaryMultipart, nil, binaryMultipartDecoded},
		{"folded CTE field relabelled whole", binaryBase64Single, nil,
			"From: a@b\r\nContent-Transfer-Encoding: binary\r\nSubject: folded\r\n\r\nhello"},
		{"part 1 of a single-part base64 message", binaryBase64Single, []int{1}, "hello"},
		{"a nested message is decoded inside", binaryNested, nil,
			"From: a@b\r\nContent-Type: message/rfc822\r\n\r\nFrom: c@d\r\nContent-Transfer-Encoding: binary\r\n\r\nhello"},
		{"a folded field after CTE stays its own", binaryFoldedAfter, nil,
			"Content-Transfer-Encoding: binary\r\nSubject: a\r\n b\r\n\r\nhello"},
		{"quoted-printable: soft break, lowercase hex, trailing space", qpMessage("a=\r\nb=c3=a9 \r\nc\nd"), []int{1},
			"abé\r\nc\r\nd"},
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

// A section that cannot be decoded fails the command: UNKNOWN-CTE for the
// encoding (RFC 3516 4.3), PARSE for bytes the encoding does not describe.
func TestFetchBinaryUndecodableIsRefused(t *testing.T) {
	uuencoded := strings.Replace(binaryMultipart, "Content-Transfer-Encoding: base64", "Content-Transfer-Encoding: x-uuencode", 1)
	corrupt := strings.Replace(binaryMultipart, "AAEC", "A@@C!", 1)
	for _, tc := range []struct {
		name string
		raw  string
		part []int
		code imap.ResponseCode
	}{
		{"unknown encoding, the part", uuencoded, []int{2}, imap.ResponseCodeUnknownCTE},
		{"unknown encoding, the whole message", uuencoded, nil, imap.ResponseCodeUnknownCTE},
		{"corrupt base64, the part", corrupt, []int{2}, imap.ResponseCodeParse},
		{"corrupt base64, the whole message", corrupt, nil, imap.ResponseCodeParse},
		{"quoted-printable, '=' not a hex pair", qpMessage("a=ZZb\r\n"), nil, imap.ResponseCodeParse},
		{"quoted-printable, one hex digit", qpMessage("a=Axb\r\n"), nil, imap.ResponseCodeParse},
		{"quoted-printable, '=' and space not ending the line", qpMessage("a= b\r\n"), nil, imap.ResponseCodeParse},
		{"quoted-printable, '=' at the end", qpMessage("abc="), []int{1}, imap.ResponseCodeParse},
		{"quoted-printable, CR without LF", qpMessage("a\rb\r\n"), nil, imap.ResponseCodeParse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := startAuthClient(t, "user@test.com", "testpass")
			defer func() { c.Logout().Wait() }() //nolint:errcheck
			appendWithFlags(t, c, "INBOX", []byte(tc.raw))
			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			_, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
				BinarySection: []*imap.FetchItemBinarySection{{Part: tc.part, Peek: true}},
			}).Collect()
			var imapErr *imap.Error
			if !errors.As(err, &imapErr) || imapErr.Code != tc.code {
				t.Errorf("FETCH answered %v, want NO [%s]", err, tc.code)
			}
		})
	}
}

// binaryTenBytes carries bytes 0..9 base64-encoded in part 2: an origin into the
// encoded text and one into the decoded bytes land on different bytes.
const binaryTenBytes = "From: a@b\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=XX\r\n" +
	"\r\n" +
	"--XX\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"text\r\n" +
	"--XX\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"AAECAwQFBgcICQ==\r\n" +
	"--XX--\r\n"

// <origin.count> cuts the decoded bytes (RFC 3516 4.2) and the reply names its
// origin (4.3); BINARY.SIZE has no partial form and stays the whole size.
func TestFetchBinaryPartial(t *testing.T) {
	whole := strings.Replace(binaryTenBytes, "Content-Transfer-Encoding: base64", "Content-Transfer-Encoding: binary", 1)
	whole = strings.Replace(whole, "AAECAwQFBgcICQ==", "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09", 1)
	for _, tc := range []struct {
		name    string
		part    []int
		partial imap.SectionPartial
		want    string
	}{
		{"part, from the start", []int{2}, imap.SectionPartial{Offset: 0, Size: 3}, "\x00\x01\x02"},
		{"part, count past the end", []int{2}, imap.SectionPartial{Offset: 3, Size: 10}, "\x03\x04\x05\x06\x07\x08\x09"},
		{"part, origin past the end", []int{2}, imap.SectionPartial{Offset: 20, Size: 4}, ""},
		// Over the relabelled field and the decoded bytes, where the encoded
		// message differs.
		{"whole message, relabelled", nil, imap.SectionPartial{Offset: int64(len(whole) - 44), Size: 30}, whole[len(whole)-44 : len(whole)-14]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := startAuthClient(t, "user@test.com", "testpass")
			defer func() { c.Logout().Wait() }() //nolint:errcheck
			appendWithFlags(t, c, "INBOX", []byte(binaryTenBytes))
			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			section := &imap.FetchItemBinarySection{Part: tc.part, Peek: true, Partial: &tc.partial}
			msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
				BinarySection:     []*imap.FetchItemBinarySection{section},
				BinarySectionSize: []*imap.FetchItemBinarySectionSize{{Part: tc.part}},
			}).Collect()
			if err != nil || len(msgs) != 1 || len(msgs[0].BinarySection) != 1 {
				t.Fatalf("FETCH = %+v, %v", msgs, err)
			}
			got := msgs[0].BinarySection[0]
			if got.Section.Partial == nil || got.Section.Partial.Offset != tc.partial.Offset {
				t.Errorf("reply section %+v, want origin %d", got.Section, tc.partial.Offset)
			}
			if string(got.Bytes) != tc.want {
				t.Errorf("BINARY%v<%d.%d> = %q, want %q", tc.part, tc.partial.Offset, tc.partial.Size, got.Bytes, tc.want)
			}
			wantSize := uint32(10)
			if tc.part == nil {
				wantSize = uint32(len(whole))
			}
			if size := msgs[0].BinarySectionSize[0].Size; size != wantSize {
				t.Errorf("BINARY.SIZE%v = %d, want %d: the size has no partial", tc.part, size, wantSize)
			}
		})
	}
}
