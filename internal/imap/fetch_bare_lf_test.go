package imap_test

import (
	"bytes"
	"crypto/ecdh"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/v2/internal/imap"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

const bareLF = "Subject: lf\n\nline one\nline two\n"

// bareLFServer is a maildir server whose INBOX holds the appended envelope
// message and, as message 2, a file put in new/ as stored, which may be
// encrypted by layer.
func bareLFServer(t *testing.T, layer *mailcrypt.Layer, stored []byte) *rawConn {
	t.Helper()
	root := t.TempDir()
	srv := imapserver.New(imapserver.Options{
		Mailbox:   maildir.New(maildir.WithCrypt(layer)),
		Index:     file.New(),
		Resolver:  &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		AuthRelay: authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
	})
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	c := dialRaw(t, ln.Addr().String())
	c.login()
	appendEnvelopeMsg(t, c)
	c.cmd(`SELECT INBOX`)
	var dir string
	_ = filepath.Walk(root, func(p string, i os.FileInfo, _ error) error {
		if i != nil && i.IsDir() && filepath.Base(p) == "new" {
			dir = p
		}
		return nil
	})
	if dir == "" {
		t.Fatal("no new/ directory")
	}
	if err := os.WriteFile(filepath.Join(dir, "1700000000.lf.host"), stored, 0o600); err != nil {
		t.Fatal(err)
	}
	c.cmd(`NOOP`)
	return c
}

// bareLFStates are one bare-LF message in the three states a maildir file has.
func bareLFStates(t *testing.T) map[string]struct {
	layer  *mailcrypt.Layer
	stored []byte
	plain  string
} {
	t.Helper()
	id, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ageL := mailcrypt.New(mailcrypt.Options{Recipients: []age.Recipient{id.Recipient()}, Identities: []age.Identity{id}})
	var sealed bytes.Buffer
	if err := ageL.Encrypt(&sealed, strings.NewReader(bareLF)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("..", "mailcrypt", "refstream", "testdata", "reference")
	pem, err := os.ReadFile(filepath.Join(dir, "X25519.pem"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := refstream.ParsePrivateKeyPEM(pem)
	if err != nil {
		t.Fatal(err)
	}
	vec, err := os.ReadFile(filepath.Join(dir, "X25519-gcm-1000.bin"))
	if err != nil {
		t.Fatal(err)
	}
	refL := mailcrypt.New(mailcrypt.Options{ReferenceKeys: []*ecdh.PrivateKey{key}})
	o, err := refL.Open(bytesBody(vec), nil)
	if err != nil {
		t.Fatal(err)
	}
	refPlain := make([]byte, o.Size())
	if _, err := o.ReadAt(refPlain, 0); err != nil && len(refPlain) != 1000 {
		t.Fatal(err)
	}
	return map[string]struct {
		layer  *mailcrypt.Layer
		stored []byte
		plain  string
	}{
		"plaintext": {mailcrypt.New(mailcrypt.Options{}), []byte(bareLF), bareLF},
		"age":       {ageL, sealed.Bytes(), bareLF},
		"reference": {refL, vec, string(refPlain)},
	}
}

type bytesBody []byte

func (b bytesBody) ReadAt(p []byte, off int64) (int, error) { return bytes.NewReader(b).ReadAt(p, off) }
func (b bytesBody) Size() int64                             { return int64(len(b)) }

func crlfOf(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' && (i == 0 || s[i-1] != '\r') {
			b.WriteByte('\r')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// fetchLiteral sends one FETCH and returns RFC822.SIZE and the literal after
// marker, read whole off the connection so CR and LF stay as sent.
func fetchLiteral(t *testing.T, c *rawConn, items, marker string) (int, string) {
	t.Helper()
	c.seq++
	tag := "z" + strconv.Itoa(c.seq)
	c.conn.Write([]byte(tag + " FETCH 2 (" + items + ")\r\n"))
	var all []byte
	buf := make([]byte, 4096)
	for !bytes.Contains(all, []byte("\r\n"+tag+" ")) {
		n, err := c.r.Read(buf)
		if err != nil {
			t.Fatalf("read: %v (so far %q)", err, all)
		}
		all = append(all, buf[:n]...)
	}
	size := -1
	if m := regexp.MustCompile(`RFC822\.SIZE (\d+)`).FindSubmatch(all); m != nil {
		size, _ = strconv.Atoi(string(m[1]))
	}
	m := regexp.MustCompile(regexp.QuoteMeta(marker) + ` \{(\d+)\}\r\n`).FindSubmatchIndex(all)
	if m == nil {
		t.Fatalf("no %s literal in %q", marker, all)
	}
	n, _ := strconv.Atoi(string(all[m[2]:m[3]]))
	return size, string(all[m[1] : m[1]+n])
}

// A bare-LF maildir message, in every state, answers BODY[] as long as its
// RFC822.SIZE and with CRLF line ends; v2.4.7 sent the bare LF and a size two
// octets larger than the body (#2581).
func TestBareLFMaildirBodyMatchesRFC822Size(t *testing.T) {
	for name, st := range bareLFStates(t) {
		t.Run(name, func(t *testing.T) {
			c := bareLFServer(t, st.layer, st.stored)
			size, body := fetchLiteral(t, c, "RFC822.SIZE BODY.PEEK[]", "BODY[]")
			if want := crlfOf(st.plain); body != want || size != len(want) {
				t.Fatalf("RFC822.SIZE %d, BODY[] %d octets (CRLF %v), want %d CRLF octets", size, len(body), body == want, len(want))
			}
		})
	}
}

// BODY[]<n.m> on a bare-LF maildir message counts the CRLF stream, as v2.4.7
// did; the random access of 2.4.8 read the file's raw offsets (#2581).
func TestBareLFMaildirPartialCountsTheCRLFStream(t *testing.T) {
	for name, st := range bareLFStates(t) {
		t.Run(name, func(t *testing.T) {
			c := bareLFServer(t, st.layer, st.stored)
			_, part := fetchLiteral(t, c, "BODY.PEEK[]<10.8>", "BODY[]<10>")
			if want := crlfOf(st.plain)[10:18]; part != want {
				t.Fatalf("BODY[]<10.8> %q, want %q", part, want)
			}
		})
	}
}
