package imap

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"
)

// binarySection is BINARY[<part>] (RFC 3516): the section decoded by its own
// transfer encoding. The whole message keeps its header, relabelled binary.
func binarySection(raw []byte, part []int) (data []byte, unknownCTE bool) {
	if len(part) == 0 {
		return binaryWhole(raw)
	}
	body := imapserver.ExtractBodySection(bytes.NewReader(raw), &imaplib.FetchItemBodySection{Part: part})
	if body == nil {
		return []byte{}, false // no such part: empty, as for BODY[<part>]
	}
	mime := imapserver.ExtractBodySection(bytes.NewReader(raw),
		&imaplib.FetchItemBodySection{Part: part, Specifier: imaplib.PartSpecifierMIME})
	decoded, _, unknown := decodeCTE(headerValue(mime, "Content-Transfer-Encoding"), body)
	return decoded, unknown
}

func binaryWhole(raw []byte) ([]byte, bool) {
	br := bufio.NewReader(bytes.NewReader(raw))
	hdr, err := textproto.ReadHeader(br)
	if err != nil {
		return raw, false
	}
	body, _ := io.ReadAll(br)
	decoded, changed, unknown := decodeCTE(hdr.Get("Content-Transfer-Encoding"), body)
	if unknown || !changed {
		return raw, unknown
	}
	var out bytes.Buffer
	out.Write(relabelBinary(raw[:len(raw)-len(body)]))
	out.Write(decoded)
	return out.Bytes(), false
}

// relabelBinary rewrites the Content-Transfer-Encoding field where it stands,
// continuation lines included; the header keeps its order and its bytes.
func relabelBinary(header []byte) []byte {
	var out bytes.Buffer
	lines := bytes.SplitAfter(header, []byte{'\n'})
	for i := 0; i < len(lines); i++ {
		name, _, found := bytes.Cut(lines[i], []byte{':'})
		if !found || !strings.EqualFold(strings.TrimSpace(string(name)), "Content-Transfer-Encoding") {
			out.Write(lines[i])
			continue
		}
		out.WriteString("Content-Transfer-Encoding: binary\r\n")
		for i+1 < len(lines) && len(lines[i+1]) > 0 && (lines[i+1][0] == ' ' || lines[i+1][0] == '\t') {
			i++
		}
	}
	return out.Bytes()
}

// decodeCTE undoes a transfer encoding; changed is false for the identity
// ones, and bytes that do not decode are passed through as they are.
func decodeCTE(cte string, body []byte) (out []byte, changed, unknown bool) {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "", "7bit", "8bit", "binary":
		return body, false, false
	case "base64":
		// RFC 2045 6.8 allows line breaks anywhere in the encoded data.
		decoded, err := base64.StdEncoding.DecodeString(string(stripWhitespace(body)))
		if err != nil {
			return body, false, false
		}
		return decoded, true, false
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return body, false, false
		}
		return decoded, true, false
	default:
		return nil, false, true
	}
}

// headerValue returns the (last) value of name in the supplied header
// block, RFC 5322 unfold-aware. Comparison is case-insensitive per spec.
func headerValue(header []byte, name string) string {
	want := strings.ToLower(name)
	var current string
	for _, line := range bytes.Split(header, []byte{'\n'}) {
		l := strings.TrimRight(string(line), "\r")
		if l == "" {
			continue
		}
		// Continuation line — append to the current value.
		if l[0] == ' ' || l[0] == '\t' {
			current += " " + strings.TrimSpace(l)
			continue
		}
		colon := strings.IndexByte(l, ':')
		if colon < 0 {
			continue
		}
		if strings.ToLower(strings.TrimSpace(l[:colon])) == want {
			current = strings.TrimSpace(l[colon+1:])
		}
	}
	return current
}

// stripWhitespace removes ASCII whitespace from b, used to undo line
// folding inside base64-encoded bodies.
func stripWhitespace(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		out = append(out, c)
	}
	return out
}
