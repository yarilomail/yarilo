package refstream

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/binary"
	"testing"
)

// encodeV2 builds a version 2 stream the way the reference writes one: one
// EC key slot, the key data wrapped with AES-256-CBC, the body in AES-256-GCM
// (AEAD) or AES-256-CBC with HMAC-SHA256. Test-only: the package never writes.
func encodeV2(t testing.TB, pub *ecdh.PublicKey, aead bool, plain []byte) []byte {
	t.Helper()
	calg := cipherSpec{"aes-256-cbc", 32, false}
	calgOID := asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	flags := uint32(flagHMAC)
	if aead {
		calg = cipherSpec{"aes-256-gcm", 32, true}
		calgOID = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 46}
		flags = flagAEAD
	}
	material := make([]byte, calg.keyLen+calg.ivLen()+tagSize)
	_, _ = rand.Read(material)
	key, iv, extra := material[:32], material[32:32+calg.ivLen()], material[32+calg.ivLen():]

	eph, err := pub.Curve().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := eph.ECDH(pub)
	if err != nil {
		t.Fatal(err)
	}
	ep := compressedPoint(eph.PublicKey())
	tk, err := pbkdf2.Key(sha256.New, string(secret), ep, 2048, 32+16)
	if err != nil {
		t.Fatal(err)
	}
	wb, _ := aes.NewCipher(tk[:32])
	padded := pad(material)
	wrapped := make([]byte, len(padded))
	cipher.NewCBCEncrypter(wb, tk[32:48]).CryptBlocks(wrapped, padded)

	id, err := keyID(pub)
	if err != nil {
		t.Fatal(err)
	}
	var kd bytes.Buffer
	kd.WriteByte(1)
	kd.WriteByte(keyTypeEC)
	kd.Write(id)
	put32(&kd, ep)
	put32(&kd, wrapped)
	put32(&kd, stretch(sha256.New, material))

	c, _ := asn1.Marshal(calgOID)
	m, _ := asn1.Marshal(asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1})
	var body []byte
	block, _ := aes.NewCipher(key)
	if aead {
		g, _ := cipher.NewGCM(block)
		body = g.Seal(nil, iv, plain, extra)
	} else {
		p := pad(plain)
		body = make([]byte, len(p))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(body, p)
		mac := hmac.New(sha256.New, extra)
		mac.Write(body)
		body = mac.Sum(body)
	}

	var out bytes.Buffer
	out.Write(Magic)
	out.WriteByte(2)
	_ = binary.Write(&out, binary.BigEndian, flags)
	total := len(Magic) + 9 + len(c) + len(m) + 8 + kd.Len()
	_ = binary.Write(&out, binary.BigEndian, uint32(total))
	out.Write(c)
	out.Write(m)
	_ = binary.Write(&out, binary.BigEndian, uint32(2048))
	_ = binary.Write(&out, binary.BigEndian, uint32(kd.Len()))
	out.Write(kd.Bytes())
	out.Write(body)
	return out.Bytes()
}

func put32(b *bytes.Buffer, v []byte) {
	_ = binary.Write(b, binary.BigEndian, uint32(len(v)))
	b.Write(v)
}

func pad(b []byte) []byte {
	p := aes.BlockSize - len(b)%aes.BlockSize
	return append(append([]byte(nil), b...), bytes.Repeat([]byte{byte(p)}, p)...)
}
