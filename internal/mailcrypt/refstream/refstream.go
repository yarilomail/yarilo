// Package refstream reads the encrypted message stream of the reference
// server, versions 1 and 2, for stores taken from its disk. It only reads:
// new mail is written in another format (MAILCRYPT.md in docs-internal).
package refstream

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
)

// Magic opens every stream.
var Magic = []byte("CRYPTED\x03\x07")

const (
	flagHMAC     = 0x1
	flagAEAD     = 0x2
	flagSameCalg = 0x10

	keyTypeRSA = 0x1
	keyTypeEC  = 0x2
	keyTypeKEM = 0x4

	tagSize   = 16
	maxHeader = 1 << 20
)

var (
	ErrNotEncrypted = errors.New("refstream: not an encrypted stream")
	ErrUnsupported  = errors.New("refstream: unsupported")
	ErrNoKey        = errors.New("refstream: no matching private key")
	ErrCorrupt      = errors.New("refstream: corrupt header")
	ErrTruncated    = errors.New("refstream: truncated stream")
	ErrIntegrity    = errors.New("refstream: integrity check failed")
)

// IsEncrypted reports whether data starts with the stream magic.
func IsEncrypted(prefix []byte) bool { return bytes.HasPrefix(prefix, Magic) }

type cipherSpec struct {
	name   string
	keyLen int
	gcm    bool
}

var cipherOIDs = map[string]cipherSpec{
	"2.16.840.1.101.3.4.1.2":  {"aes-128-cbc", 16, false},
	"2.16.840.1.101.3.4.1.22": {"aes-192-cbc", 24, false},
	"2.16.840.1.101.3.4.1.42": {"aes-256-cbc", 32, false},
	"2.16.840.1.101.3.4.1.6":  {"aes-128-gcm", 16, true},
	"2.16.840.1.101.3.4.1.26": {"aes-192-gcm", 24, true},
	"2.16.840.1.101.3.4.1.46": {"aes-256-gcm", 32, true},
}

func (c cipherSpec) ivLen() int {
	if c.gcm {
		return 12
	}
	return aes.BlockSize
}

var hashOIDs = map[string]func() hash.Hash{
	"2.16.840.1.101.3.4.2.1": sha256.New,
	"2.16.840.1.101.3.4.2.2": sha512.New384,
	"2.16.840.1.101.3.4.2.3": sha512.New,
}

// NewReader reads the header from src and returns a reader of the plaintext.
// The key is chosen among keys by the id the header names. A failed integrity
// check at the end is returned by Read as ErrIntegrity, never as io.EOF.
func NewReader(src io.Reader, keys ...*ecdh.PrivateKey) (io.Reader, error) {
	head := make([]byte, len(Magic)+1)
	if _, err := io.ReadFull(src, head); err != nil {
		return nil, headErr(err)
	}
	if !IsEncrypted(head) {
		return nil, ErrNotEncrypted
	}
	switch head[len(Magic)] {
	case 1:
		return readV1(src, keys)
	case 2:
		return readV2(src, keys)
	default:
		return nil, fmt.Errorf("refstream: version 0x%02x: %w", head[len(Magic)], ErrUnsupported)
	}
}

func headErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrTruncated
	}
	return err
}

// field reads big-endian length-prefixed fields out of a header.
type field struct {
	b   []byte
	bad bool
}

func (f *field) u32() uint32 {
	if len(f.b) < 4 {
		f.bad = true
		return 0
	}
	v := binary.BigEndian.Uint32(f.b)
	f.b = f.b[4:]
	return v
}

func (f *field) take(n uint64) []byte {
	if uint64(len(f.b)) < n {
		f.bad = true
		return nil
	}
	v := f.b[:n]
	f.b = f.b[n:]
	return v
}

func (f *field) bytes32() []byte { return f.take(uint64(f.u32())) }

func (f *field) bytes16() []byte {
	if len(f.b) < 2 {
		f.bad = true
		return nil
	}
	n := binary.BigEndian.Uint16(f.b)
	f.b = f.b[2:]
	return f.take(uint64(n))
}

// oid reads a DER OBJECT IDENTIFIER.
func (f *field) oid() string {
	var id asn1.ObjectIdentifier
	rest, err := asn1.Unmarshal(f.b, &id)
	if err != nil {
		f.bad = true
		return ""
	}
	f.b = rest
	return id.String()
}

func readV2(src io.Reader, keys []*ecdh.PrivateKey) (io.Reader, error) {
	var fixed [8]byte
	if _, err := io.ReadFull(src, fixed[:]); err != nil {
		return nil, headErr(err)
	}
	flags := binary.BigEndian.Uint32(fixed[:4])
	total := binary.BigEndian.Uint32(fixed[4:])
	consumed := uint32(len(Magic) + 1 + 8)
	if total < consumed || total > maxHeader {
		return nil, fmt.Errorf("refstream: header length %d: %w", total, ErrCorrupt)
	}
	hdr := make([]byte, total-consumed)
	if _, err := io.ReadFull(src, hdr); err != nil {
		return nil, headErr(err)
	}
	f := &field{b: hdr}
	calgOID, malgOID := f.oid(), f.oid()
	rounds := f.u32()
	keyData := f.bytes32()
	if f.bad {
		return nil, ErrCorrupt
	}
	calg, ok := cipherOIDs[calgOID]
	if !ok {
		return nil, fmt.Errorf("refstream: cipher %s: %w", calgOID, ErrUnsupported)
	}
	newHash, ok := hashOIDs[malgOID]
	if !ok {
		return nil, fmt.Errorf("refstream: hash %s: %w", malgOID, ErrUnsupported)
	}
	integrity := 0
	switch {
	case flags&flagHMAC != 0:
		integrity = flagHMAC
	case flags&flagAEAD != 0:
		if !calg.gcm {
			return nil, fmt.Errorf("refstream: AEAD with %s: %w", calg.name, ErrUnsupported)
		}
		integrity = flagAEAD
	case calg.gcm:
		return nil, fmt.Errorf("refstream: %s without its tag: %w", calg.name, ErrUnsupported)
	}
	tsz := 0
	if integrity != 0 {
		tsz = tagSize
	}
	kl := calg.keyLen + calg.ivLen() + tsz
	material, err := unwrapV2(keyData, keys, calg, newHash, int(rounds), flags, kl)
	if err != nil {
		return nil, err
	}
	key, iv, extra := material[:calg.keyLen], material[calg.keyLen:calg.keyLen+calg.ivLen()], material[calg.keyLen+calg.ivLen():]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	switch {
	case integrity == flagAEAD:
		return &bodyReader{src: newTail(src, tagSize), gcm: newGCMStream(block, iv, extra)}, nil
	case integrity == flagHMAC:
		mac := hmac.New(newHash, extra)
		return &bodyReader{src: newTail(src, mac.Size()), cbc: cipher.NewCBCDecrypter(block, iv), mac: mac}, nil
	default:
		return &bodyReader{src: newTail(src, 0), cbc: cipher.NewCBCDecrypter(block, iv)}, nil
	}
}

// unwrapV2 finds the key slot of one of keys and returns the stream key, IV
// and integrity material it wraps.
func unwrapV2(data []byte, keys []*ecdh.PrivateKey, calg cipherSpec, newHash func() hash.Hash, rounds int, flags uint32, kl int) ([]byte, error) {
	f := &field{b: data}
	n := f.take(1)
	if f.bad || n[0] == 0 {
		return nil, ErrCorrupt
	}
	type slot struct {
		ktype     byte
		id, ep, e []byte
	}
	slots := make([]slot, 0, n[0])
	for range int(n[0]) {
		var s slot
		t := f.take(1)
		s.id = f.take(32)
		s.ep = f.bytes32()
		s.e = f.bytes32()
		if f.bad {
			return nil, ErrCorrupt
		}
		s.ktype = t[0]
		slots = append(slots, s)
	}
	ekhash := f.bytes32()
	if f.bad {
		return nil, ErrCorrupt
	}
	for _, s := range slots {
		switch s.ktype {
		case keyTypeEC:
		case keyTypeRSA:
			return nil, fmt.Errorf("refstream: RSA key slot: %w", ErrUnsupported)
		case keyTypeKEM:
			return nil, fmt.Errorf("refstream: ML-KEM key slot: %w", ErrUnsupported)
		default:
			return nil, fmt.Errorf("refstream: key type 0x%02x: %w", s.ktype, ErrUnsupported)
		}
		for _, k := range keys {
			id, err := keyID(k.PublicKey())
			if err != nil || subtle.ConstantTimeCompare(id, s.id) != 1 {
				continue
			}
			material, err := unwrapSlot(k, s.ep, s.e, calg, newHash, rounds, flags, kl)
			if err != nil {
				return nil, err
			}
			if !hmac.Equal(stretch(newHash, material), ekhash) {
				return nil, fmt.Errorf("refstream: key data hash: %w", ErrCorrupt)
			}
			return material, nil
		}
	}
	return nil, ErrNoKey
}

func unwrapSlot(k *ecdh.PrivateKey, ep, wrapped []byte, calg cipherSpec, newHash func() hash.Hash, rounds int, flags uint32, kl int) ([]byte, error) {
	peer, err := peerKey(k.Curve(), ep)
	if err != nil {
		return nil, fmt.Errorf("refstream: ephemeral key: %w", ErrCorrupt)
	}
	secret, err := k.ECDH(peer)
	if err != nil {
		return nil, fmt.Errorf("refstream: key agreement: %w", ErrCorrupt)
	}
	wrap := cipherSpec{"aes-256-cbc", 32, false}
	if flags&flagSameCalg != 0 {
		wrap = calg
	}
	aadLen := 0
	if wrap.gcm && flags&flagAEAD != 0 {
		aadLen = tagSize
	}
	t, err := pbkdf2.Key(newHash, string(secret), ep, rounds, wrap.keyLen+wrap.ivLen()+aadLen)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(t[:wrap.keyLen])
	if err != nil {
		return nil, err
	}
	iv := t[wrap.keyLen : wrap.keyLen+wrap.ivLen()]
	var out []byte
	if wrap.gcm {
		if aadLen == 0 {
			return nil, fmt.Errorf("refstream: key wrap %s without its tag: %w", wrap.name, ErrUnsupported)
		}
		g, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		if out, err = g.Open(nil, iv, wrapped, t[wrap.keyLen+wrap.ivLen():]); err != nil {
			return nil, ErrNoKey
		}
	} else {
		if len(wrapped) == 0 || len(wrapped)%aes.BlockSize != 0 {
			return nil, ErrCorrupt
		}
		out = make([]byte, len(wrapped))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, wrapped)
		if out, err = unpad(out); err != nil {
			return nil, ErrNoKey
		}
	}
	if len(out) != kl {
		return nil, fmt.Errorf("refstream: key data length %d, want %d: %w", len(out), kl, ErrCorrupt)
	}
	return out, nil
}

// stretch is the key data check value: H(data), then 2048 rounds of
// H(prev || be32(i)).
func stretch(newHash func() hash.Hash, data []byte) []byte {
	h := newHash()
	h.Write(data)
	sum := h.Sum(nil)
	var i32 [4]byte
	for i := uint32(1); i < 2049; i++ {
		h.Reset()
		h.Write(sum)
		binary.BigEndian.PutUint32(i32[:], i)
		h.Write(i32[:])
		sum = h.Sum(sum[:0])
	}
	return sum
}

func readV1(src io.Reader, keys []*ecdh.PrivateKey) (io.Reader, error) {
	var l [2]byte
	if _, err := io.ReadFull(src, l[:]); err != nil {
		return nil, headErr(err)
	}
	hdr := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(src, hdr); err != nil {
		return nil, headErr(err)
	}
	f := &field{b: hdr}
	ep, pkID, keyDigest, wrapped := f.bytes16(), f.bytes16(), f.bytes16(), f.bytes16()
	if f.bad || len(ep) == 0 || len(pkID) == 0 || len(wrapped) == 0 {
		return nil, ErrCorrupt
	}
	for _, k := range keys {
		if k.Curve() == ecdh.X25519() {
			continue
		}
		id := keyIDv1(k.PublicKey())
		n := min(len(id), len(pkID))
		if subtle.ConstantTimeCompare(id[:n], pkID[:n]) != 1 {
			continue
		}
		peer, err := peerKey(k.Curve(), ep)
		if err != nil {
			return nil, fmt.Errorf("refstream: ephemeral key: %w", ErrCorrupt)
		}
		secret, err := k.ECDH(peer)
		if err != nil {
			return nil, fmt.Errorf("refstream: key agreement: %w", ErrCorrupt)
		}
		kek := sha256.Sum256(secret)
		key := ctrZeroIV(kek[:], wrapped)
		if sum := sha256.Sum256(key); len(keyDigest) != len(sum) || !hmac.Equal(sum[:], keyDigest) {
			return nil, fmt.Errorf("refstream: decrypted key is invalid: %w", ErrCorrupt)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("refstream: key length %d: %w", len(key), ErrCorrupt)
		}
		var zero [aes.BlockSize]byte
		return &bodyReader{src: newTail(src, 0), ctr: cipher.NewCTR(block, zero[:])}, nil
	}
	return nil, ErrNoKey
}

// ctrZeroIV is AES-CTR with an all-zero IV, as version 1 uses for both the
// wrapped key and the body.
func ctrZeroIV(key, data []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	var zero [aes.BlockSize]byte
	out := make([]byte, len(data))
	cipher.NewCTR(block, zero[:]).XORKeyStream(out, data)
	return out
}

func unpad(b []byte) ([]byte, error) {
	if len(b) == 0 || len(b)%aes.BlockSize != 0 {
		return nil, ErrIntegrity
	}
	p := int(b[len(b)-1])
	if p == 0 || p > aes.BlockSize || p > len(b) {
		return nil, ErrIntegrity
	}
	for _, c := range b[len(b)-p:] {
		if int(c) != p {
			return nil, ErrIntegrity
		}
	}
	return b[:len(b)-p], nil
}
