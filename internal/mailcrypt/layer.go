package mailcrypt

import (
	"bufio"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"os"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/agestream"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

// ErrPlanted refuses a body that already carries an encryption signature
// yarilo did not put there.
var ErrPlanted = errors.New("mailcrypt: body already carries an encryption signature")

// ErrNoKey is a body whose key is not configured.
var ErrNoKey = errors.New("mailcrypt: no key for this body")

// Options configure a Layer; the zero value writes plaintext and still
// refuses planted ciphertext.
type Options struct {
	// Recipients encrypt new bodies; none writes plaintext.
	Recipients []age.Recipient
	// Identities open age bodies; several during a key rotation.
	Identities []age.Identity
	// ReferenceKeys open bodies in the reference format.
	ReferenceKeys []*ecdh.PrivateKey
	// Cache holds decrypted reference-format bodies; nil streams them.
	Cache *Cache
}

// Layer seals bodies on their way to a storage driver and opens them on the
// way back. It is safe for concurrent use.
type Layer struct{ o Options }

// New returns a Layer; a nil *Layer behaves as New(Options{}).
func New(o Options) *Layer { return &Layer{o: o} }

func (l *Layer) opts() Options {
	if l == nil {
		return Options{}
	}
	return l.o
}

// Writes reports whether new bodies are encrypted.
func (l *Layer) Writes() bool { return len(l.opts().Recipients) > 0 }

// Guard refuses a body that starts with either signature and returns a
// reader of the whole body; a driver that rewrites line ends calls it on the
// raw input, before the rewrite could hide a signature.
func Guard(src io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(src, 4096)
	head, err := br.Peek(PrefixLen)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, err
	}
	if Classify(head) != Plain {
		return nil, ErrPlanted
	}
	return br, nil
}

// Seal copies src into dst, encrypted when the layer writes, and refuses a
// body that starts with either signature in both modes.
func (l *Layer) Seal(dst io.Writer, src io.Reader) error {
	br, err := Guard(src)
	if err != nil {
		return err
	}
	if !l.Writes() {
		_, err := io.Copy(dst, br)
		return err
	}
	w, err := agestream.NewWriter(dst, l.opts().Recipients...)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, br); err != nil {
		return err
	}
	return w.Close()
}

// Encrypt copies src into dst, encrypted when the layer writes, with no
// signature check: for blobs, which may be any bytes, never for message
// bodies (Seal).
func (l *Layer) Encrypt(dst io.Writer, src io.Reader) error {
	if !l.Writes() {
		_, err := io.Copy(dst, src)
		return err
	}
	w, err := agestream.NewWriter(dst, l.opts().Recipients...)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, src); err != nil {
		return err
	}
	return w.Close()
}

// Body is a stored body: random access over its bytes and its length.
type Body interface {
	io.ReaderAt
	Size() int64
}

// Opened is a plaintext body: a stream, random access, its size and Close.
// It satisfies mailbox.RangeReader.
type Opened interface {
	io.ReadCloser
	io.ReaderAt
	Size() int64
}

// Open returns the plaintext of body; closer is closed with the result.
func (l *Layer) Open(body Body, closer io.Closer) (Opened, error) {
	head := make([]byte, PrefixLen)
	n, err := body.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, closeWith(closer, err)
	}
	switch Classify(head[:n]) {
	case Age:
		ra, size, err := agestream.NewReaderAt(body, body.Size(), l.opts().Identities...)
		if err != nil {
			return nil, closeWith(closer, keyErr(err))
		}
		return newOpened(ra, size, closer), nil
	case Reference:
		return l.openReference(body, closer)
	}
	return newOpened(body, body.Size(), closer), nil
}

// PlainSize is the plaintext length of body: from the header and the length
// for age, with no key; by decrypting for the reference format; the length
// itself for plaintext.
func (l *Layer) PlainSize(body Body) (int64, error) {
	head := make([]byte, PrefixLen)
	n, err := body.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	switch Classify(head[:n]) {
	case Age:
		return agestream.PlainSize(body, body.Size())
	case Reference:
		o, err := l.Open(body, nil)
		if err != nil {
			return 0, err
		}
		defer o.Close()
		return o.Size(), nil
	}
	return body.Size(), nil
}

func (l *Layer) openReference(body Body, closer io.Closer) (Opened, error) {
	if c := l.opts().Cache; c != nil {
		o, err := c.open(body, l.opts().ReferenceKeys)
		if err == nil {
			return withCloser(o, closer), nil
		}
		if !errors.Is(err, errBypass) {
			return nil, closeWith(closer, keyErr(err))
		}
	}
	rs, err := newRefStream(body, l.opts().ReferenceKeys, closer)
	if err != nil {
		return nil, closeWith(closer, keyErr(err))
	}
	return rs, nil
}

func keyErr(err error) error {
	var noMatch *age.NoIdentityMatchError
	if errors.As(err, &noMatch) || errors.Is(err, refstream.ErrNoKey) {
		return fmt.Errorf("%w: %w", ErrNoKey, err)
	}
	return err
}

func closeWith(c io.Closer, err error) error {
	if c != nil {
		_ = c.Close()
	}
	return err
}

// opened streams a ReaderAt from offset 0 and keeps random access.
type opened struct {
	*io.SectionReader
	closer io.Closer
}

func newOpened(ra io.ReaderAt, size int64, closer io.Closer) *opened {
	return &opened{SectionReader: io.NewSectionReader(ra, 0, size), closer: closer}
}

func (o *opened) Close() error {
	if o.closer == nil {
		return nil
	}
	return o.closer.Close()
}

type closing struct {
	Opened
	extra io.Closer
}

func withCloser(o Opened, c io.Closer) Opened { return &closing{Opened: o, extra: c} }

func (c *closing) Close() error {
	err := c.Opened.Close()
	if c.extra != nil {
		if e := c.extra.Close(); err == nil {
			err = e
		}
	}
	return err
}

// fileBody is a whole file as a Body.
type fileBody struct {
	*os.File
	size int64
}

func (f fileBody) Size() int64 { return f.size }

// OpenFile opens path and returns its plaintext; the file closes with it.
func (l *Layer) OpenFile(path string) (Opened, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return l.Open(fileBody{File: f, size: st.Size()}, f)
}

// PlainFileSize is the plaintext length of the file at path, size bytes on
// disk (PlainSize).
func (l *Layer) PlainFileSize(path string, size int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return l.PlainSize(fileBody{File: f, size: size})
}
