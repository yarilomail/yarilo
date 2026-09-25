package imap

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

var (
	errUnknownCTE  = errors.New("unknown Content-Transfer-Encoding")
	errInvalidMIME = errors.New("invalid data in MIME part")
)

// binarySection is BINARY[<part>] (RFC 3516): each leaf decoded by its own
// encoding and relabelled binary; the whole message keeps its header.
func binarySection(raw []byte, part []int) ([]byte, error) {
	if len(part) == 0 {
		return binaryEntity(raw, true)
	}
	body := imapserver.ExtractBodySection(bytes.NewReader(raw), &imaplib.FetchItemBodySection{Part: part})
	if body == nil {
		return []byte{}, nil // no such part: empty, as for BODY[<part>]
	}
	hdr := imapserver.ExtractBodySection(bytes.NewReader(raw),
		&imaplib.FetchItemBodySection{Part: part, Specifier: imaplib.PartSpecifierMIME})
	return binaryEntity(append(append([]byte{}, hdr...), body...), false)
}

// binaryEntity decodes one MIME entity, header then body; withHeader=false
// answers the body alone.
func binaryEntity(entity []byte, withHeader bool) ([]byte, error) {
	header, body := splitHeader(entity)
	cte := strings.ToLower(strings.TrimSpace(headerValue(header, "Content-Transfer-Encoding")))
	var out bytes.Buffer
	converted := cte == "base64" || cte == "quoted-printable"
	if withHeader {
		if converted {
			out.Write(relabelBinary(header))
		} else {
			out.Write(header)
		}
	}
	mediaType, params, _ := mime.ParseMediaType(headerValue(header, "Content-Type"))
	switch {
	case cte != "" && cte != "7bit" && cte != "8bit" && cte != "binary" && !converted:
		return nil, errUnknownCTE
	case strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" && !converted:
		if err := binaryMultipart(&out, body, params["boundary"]); err != nil {
			return nil, err
		}
	case mediaType == "message/rfc822" && !converted:
		inner, err := binaryEntity(body, true)
		if err != nil {
			return nil, err
		}
		out.Write(inner)
	default:
		decoded, err := decodeCTE(cte, body)
		if err != nil {
			return nil, err
		}
		out.Write(decoded)
	}
	return out.Bytes(), nil
}

// binaryMultipart copies preamble, delimiters and epilogue as they are and
// decodes each body part; the line break before a delimiter belongs to it.
func binaryMultipart(out *bytes.Buffer, body []byte, boundary string) error {
	dash := []byte("--" + boundary)
	var part []byte
	inPart, closed := false, false
	flush := func() error {
		content, lineBreak := trimLineBreak(part)
		decoded, err := binaryEntity(content, true)
		if err != nil {
			return err
		}
		out.Write(decoded)
		out.Write(lineBreak)
		part = part[:0]
		return nil
	}
	for _, line := range bytes.SplitAfter(body, []byte{'\n'}) {
		if closed {
			out.Write(line)
			continue
		}
		trimmed := bytes.TrimRight(line, " \t\r\n")
		if !bytes.HasPrefix(trimmed, dash) {
			if inPart {
				part = append(part, line...)
			} else {
				out.Write(line)
			}
			continue
		}
		rest := trimmed[len(dash):]
		if len(rest) != 0 && !bytes.Equal(rest, []byte("--")) {
			if inPart {
				part = append(part, line...)
			} else {
				out.Write(line)
			}
			continue
		}
		if inPart {
			if err := flush(); err != nil {
				return err
			}
		}
		out.Write(line)
		inPart, closed = len(rest) == 0, len(rest) != 0
	}
	if inPart {
		return flush() // no closing delimiter: the last part runs to the end
	}
	return nil
}

func trimLineBreak(b []byte) (content, lineBreak []byte) {
	switch {
	case bytes.HasSuffix(b, []byte("\r\n")):
		return b[:len(b)-2], b[len(b)-2:]
	case bytes.HasSuffix(b, []byte("\n")):
		return b[:len(b)-1], b[len(b)-1:]
	}
	return b, nil
}

// splitHeader cuts an entity after the blank line that ends its header; an
// entity without one is all header.
func splitHeader(entity []byte) (header, body []byte) {
	if bytes.HasPrefix(entity, []byte("\r\n")) {
		return entity[:2], entity[2:]
	}
	if bytes.HasPrefix(entity, []byte("\n")) {
		return entity[:1], entity[1:]
	}
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if i := bytes.Index(entity, sep); i >= 0 {
			return entity[:i+len(sep)], entity[i+len(sep):]
		}
	}
	return entity, nil
}

func decodeCTE(cte string, body []byte) ([]byte, error) {
	switch cte {
	case "base64":
		// RFC 2045 6.8 allows line breaks anywhere in the encoded data.
		decoded, err := base64.StdEncoding.DecodeString(string(stripWhitespace(body)))
		if err != nil {
			return nil, errInvalidMIME
		}
		return decoded, nil
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return nil, errInvalidMIME
		}
		return decoded, nil
	}
	return body, nil
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

// headerValue returns the (last) value of name in the supplied header
// block, RFC 5322 unfold-aware. Comparison is case-insensitive per spec.
func headerValue(header []byte, name string) string {
	want := strings.ToLower(name)
	var current string
	var inWanted bool
	for _, line := range bytes.Split(header, []byte{'\n'}) {
		l := strings.TrimRight(string(line), "\r")
		if l == "" {
			continue
		}
		if l[0] == ' ' || l[0] == '\t' {
			if inWanted {
				current += " " + strings.TrimSpace(l)
			}
			continue
		}
		inWanted = false
		colon := strings.IndexByte(l, ':')
		if colon < 0 {
			continue
		}
		if strings.ToLower(strings.TrimSpace(l[:colon])) == want {
			current = strings.TrimSpace(l[colon+1:])
			inWanted = true
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
