package refstream

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"errors"
	"hash"
	"io"
)

// tail reads src holding back its last n bytes, which become the footer at EOF.
type tail struct {
	src  io.Reader
	n    int
	buf  []byte
	eof  bool
	read [32 << 10]byte
}

func newTail(src io.Reader, n int) *tail { return &tail{src: src, n: n} }

// next returns body bytes that are certainly not footer; at the end it
// returns io.EOF and footer() holds the last n bytes.
func (t *tail) next() ([]byte, error) {
	for {
		if len(t.buf) > t.n {
			out := t.buf[:len(t.buf)-t.n]
			t.buf = append([]byte(nil), t.buf[len(t.buf)-t.n:]...)
			return out, nil
		}
		if t.eof {
			if len(t.buf) < t.n {
				return nil, ErrTruncated
			}
			return nil, io.EOF
		}
		m, err := t.src.Read(t.read[:])
		t.buf = append(t.buf, t.read[:m]...)
		if errors.Is(err, io.EOF) {
			t.eof = true
		} else if err != nil {
			return nil, err
		}
	}
}

func (t *tail) footer() []byte { return t.buf }

// bodyReader decrypts the body with exactly one of gcm, cbc or ctr.
type bodyReader struct {
	src *tail
	gcm *gcmStream
	cbc cipher.BlockMode
	ctr cipher.Stream
	mac hash.Hash

	pending []byte // ciphertext short of a CBC block
	held    []byte // last decrypted CBC block, unpadded at EOF
	out     []byte
	err     error
}

func (r *bodyReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		r.fill()
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func (r *bodyReader) fill() {
	ct, err := r.src.next()
	if errors.Is(err, io.EOF) {
		r.err = r.finish()
		return
	}
	if err != nil {
		r.err = err
		return
	}
	if r.mac != nil {
		r.mac.Write(ct)
	}
	switch {
	case r.gcm != nil:
		pt := make([]byte, len(ct))
		r.gcm.decrypt(pt, ct)
		r.out = pt
	case r.ctr != nil:
		pt := make([]byte, len(ct))
		r.ctr.XORKeyStream(pt, ct)
		r.out = pt
	default:
		r.pending = append(r.pending, ct...)
		full := len(r.pending) / aes.BlockSize * aes.BlockSize
		if full == 0 {
			return
		}
		pt := make([]byte, full)
		r.cbc.CryptBlocks(pt, r.pending[:full])
		r.pending = append([]byte(nil), r.pending[full:]...)
		pt = append(r.held, pt...)
		r.held = append([]byte(nil), pt[len(pt)-aes.BlockSize:]...)
		r.out = pt[:len(pt)-aes.BlockSize]
	}
}

// finish checks integrity and releases the last CBC block, or fails.
func (r *bodyReader) finish() error {
	footer := r.src.footer()
	switch {
	case r.gcm != nil:
		if !r.gcm.verify(footer) {
			return ErrIntegrity
		}
		return io.EOF
	case r.ctr != nil:
		return io.EOF
	}
	if r.mac != nil && !hmac.Equal(r.mac.Sum(nil), footer) {
		return ErrIntegrity
	}
	if len(r.pending) != 0 {
		return ErrTruncated
	}
	if r.held == nil {
		return ErrTruncated
	}
	last, err := unpad(r.held)
	if err != nil {
		return err
	}
	r.held = nil
	r.out = last
	return io.EOF
}
