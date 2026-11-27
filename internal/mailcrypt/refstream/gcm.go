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
	table  [16][2]uint64
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
	g.table = ghashTable([2]uint64{binary.BigEndian.Uint64(hb[:8]), binary.BigEndian.Uint64(hb[8:])})
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
	g.y = ghashMul(&g.table, g.y)
}

// ghashTable holds the 16 multiples of H by 4-bit values, bit-reversed as
// GCM orders them; ghashMul then takes four bits a step.
func ghashTable(h [2]uint64) [16][2]uint64 {
	var t [16][2]uint64
	t[reverse4(1)] = h
	for i := 2; i < 16; i += 2 {
		t[reverse4(i)] = double(t[reverse4(i/2)])
		a, b := t[reverse4(i)], h
		t[reverse4(i+1)] = [2]uint64{a[0] ^ b[0], a[1] ^ b[1]}
	}
	return t
}

func reverse4(i int) int {
	i = (i<<2)&0xc | (i>>2)&0x3
	return (i<<1)&0xa | (i>>1)&0x5
}

func double(x [2]uint64) [2]uint64 {
	carry := x[1] & 1
	x[1] = x[1]>>1 | x[0]<<63
	x[0] >>= 1
	x[0] ^= 0xe100000000000000 & -carry
	return x
}

var ghashReduction = [16]uint16{
	0x0000, 0x1c20, 0x3840, 0x2460, 0x7080, 0x6ca0, 0x48c0, 0x54e0,
	0xe100, 0xfd20, 0xd940, 0xc560, 0x9180, 0x8da0, 0xa9c0, 0xb5e0,
}

func ghashMul(t *[16][2]uint64, y [2]uint64) [2]uint64 {
	var z [2]uint64
	for i := 1; i >= 0; i-- {
		word := y[i]
		for range 16 {
			msw := z[1] & 0xf
			z[1] = z[1]>>4 | z[0]<<60
			z[0] >>= 4
			z[0] ^= uint64(ghashReduction[msw]) << 48
			e := t[word&0xf]
			z[0] ^= e[0]
			z[1] ^= e[1]
			word >>= 4
		}
	}
	return z
}
