package refstream

import (
	"crypto/cipher"
	"crypto/subtle"
	"encoding/binary"
)

// gcmStream decrypts AES-GCM as a stream: plaintext is released as it is
// read and the tag is checked at the end (NIST SP 800-38D, 96-bit IV).
type gcmStream struct {
	ctr    cipher.Stream
	tagKey [16]byte // E(K, J0)
	h      [2]uint64
	y      [2]uint64
	part   [16]byte
	nPart  int
	aadLen uint64
	ctLen  uint64
}

func newGCMStream(block cipher.Block, iv, aad []byte) *gcmStream {
	g := &gcmStream{}
	var zero, hb [16]byte
	block.Encrypt(hb[:], zero[:])
	g.h = [2]uint64{binary.BigEndian.Uint64(hb[:8]), binary.BigEndian.Uint64(hb[8:])}
	var j0 [16]byte
	copy(j0[:], iv)
	j0[15] = 1
	block.Encrypt(g.tagKey[:], j0[:])
	binary.BigEndian.PutUint32(j0[12:], 2)
	g.ctr = cipher.NewCTR(block, j0[:])
	g.absorb(aad)
	g.flush()
	g.aadLen = uint64(len(aad))
	return g
}

// decrypt hashes the ciphertext and writes the plaintext into dst.
func (g *gcmStream) decrypt(dst, src []byte) {
	g.absorb(src)
	g.ctLen += uint64(len(src))
	g.ctr.XORKeyStream(dst, src)
}

// verify compares the computed tag with the one at the end of the stream.
func (g *gcmStream) verify(tag []byte) bool {
	g.flush()
	var l [16]byte
	binary.BigEndian.PutUint64(l[:8], g.aadLen*8)
	binary.BigEndian.PutUint64(l[8:], g.ctLen*8)
	g.block(l[:])
	var s [16]byte
	binary.BigEndian.PutUint64(s[:8], g.y[0])
	binary.BigEndian.PutUint64(s[8:], g.y[1])
	for i := range s {
		s[i] ^= g.tagKey[i]
	}
	return len(tag) == 16 && subtle.ConstantTimeCompare(s[:], tag) == 1
}

func (g *gcmStream) absorb(b []byte) {
	for len(b) > 0 {
		n := copy(g.part[g.nPart:], b)
		g.nPart += n
		b = b[n:]
		if g.nPart == 16 {
			g.block(g.part[:])
			g.nPart = 0
		}
	}
}

// flush hashes a partial block padded with zeros.
func (g *gcmStream) flush() {
	if g.nPart == 0 {
		return
	}
	clear(g.part[g.nPart:])
	g.block(g.part[:])
	g.nPart = 0
}

func (g *gcmStream) block(b []byte) {
	g.y[0] ^= binary.BigEndian.Uint64(b[:8])
	g.y[1] ^= binary.BigEndian.Uint64(b[8:16])
	g.y = gfMul(g.y, g.h)
}

// gfMul multiplies in GF(2^128) with the GCM bit order, without branches on
// secret bits.
func gfMul(x, h [2]uint64) [2]uint64 {
	var z [2]uint64
	v := h
	for i := range 128 {
		bit := (x[i/64] >> (63 - uint(i%64))) & 1
		mask := -bit
		z[0] ^= v[0] & mask
		z[1] ^= v[1] & mask
		lsb := v[1] & 1
		v[1] = v[1]>>1 | v[0]<<63
		v[0] >>= 1
		v[0] ^= 0xe100000000000000 & -lsb
	}
	return z
}
