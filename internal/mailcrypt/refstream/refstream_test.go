package refstream

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func genKey(t testing.TB, c ecdh.Curve) *ecdh.PrivateKey {
	t.Helper()
	k, err := c.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	curves := []struct {
		name  string
		curve ecdh.Curve
	}{{"P-256", ecdh.P256()}, {"P-384", ecdh.P384()}, {"P-521", ecdh.P521()}, {"X25519", ecdh.X25519()}}
	sizes := []int{0, 1, 15, 16, 17, 4096, 65537}
	for _, c := range curves {
		k := genKey(t, c.curve)
		for _, aead := range []bool{true, false} {
			for _, n := range sizes {
				plain := make([]byte, n)
				_, _ = rand.Read(plain)
				stream := encodeV2(t, k.PublicKey(), aead, plain)
				r, err := NewReader(iotest.OneByteReader(bytes.NewReader(stream)), genKey(t, c.curve), k)
				if err != nil {
					t.Fatalf("%s aead=%v n=%d: %v", c.name, aead, n, err)
				}
				got, err := io.ReadAll(r)
				if err != nil || !bytes.Equal(got, plain) {
					t.Fatalf("%s aead=%v n=%d: err=%v equal=%v", c.name, aead, n, err, bytes.Equal(got, plain))
				}
			}
		}
	}
}

func TestGCMStreamMatchesStdlib(t *testing.T) {
	for _, n := range []int{0, 1, 15, 16, 17, 31, 32, 33, 1000} {
		key, iv, aad := make([]byte, 32), make([]byte, 12), make([]byte, 16)
		plain := make([]byte, n)
		for _, b := range [][]byte{key, iv, aad, plain} {
			_, _ = rand.Read(b)
		}
		block, _ := aes.NewCipher(key)
		g, _ := cipher.NewGCM(block)
		sealed := g.Seal(nil, iv, plain, aad)
		ct, tag := sealed[:n], sealed[n:]

		s := newGCMStream(block, iv, aad)
		out := make([]byte, n)
		for i := 0; i < n; i += 7 {
			j := min(i+7, n)
			s.decrypt(out[i:j], ct[i:j])
		}
		if !bytes.Equal(out, plain) || !s.verify(tag) {
			t.Fatalf("n=%d: plaintext equal=%v", n, bytes.Equal(out, plain))
		}
		bad := append([]byte(nil), tag...)
		bad[0] ^= 1
		s = newGCMStream(block, iv, aad)
		s.decrypt(make([]byte, n), ct)
		if s.verify(bad) {
			t.Fatalf("n=%d: a flipped tag verified", n)
		}
	}
}

func TestRefused(t *testing.T) {
	k := genKey(t, ecdh.P256())
	plain := bytes.Repeat([]byte("secret mail body\n"), 300)
	gcm := encodeV2(t, k.PublicKey(), true, plain)
	cbc := encodeV2(t, k.PublicKey(), false, plain)
	hdrEnd := len(gcm) - len(plain) - tagSize

	flip := func(b []byte, i int) []byte { c := append([]byte(nil), b...); c[i] ^= 0x40; return c }
	replace := func(b []byte, old, repl []byte) []byte { return bytes.Replace(b, old, repl, 1) }
	gcmOID, _ := asn1.Marshal(asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 46})
	ofb, _ := asn1.Marshal(asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 43})
	rounds := len(Magic) + 9 + len(gcmOID) + 11
	keySlot := rounds + 8 + 1

	cases := []struct {
		name   string
		stream []byte
		keys   []*ecdh.PrivateKey
		want   error
	}{
		{"plain text", plain, []*ecdh.PrivateKey{k}, ErrNotEncrypted},
		{"magic cut short", Magic[:5], []*ecdh.PrivateKey{k}, ErrTruncated},
		{"unknown version", replace(gcm, append(append([]byte(nil), Magic...), 2), append(append([]byte(nil), Magic...), 9)), []*ecdh.PrivateKey{k}, ErrUnsupported},
		{"truncated header", gcm[:hdrEnd-5], []*ecdh.PrivateKey{k}, ErrTruncated},
		{"truncated body", gcm[:hdrEnd+100], []*ecdh.PrivateKey{k}, ErrIntegrity},
		{"truncated before the tag", gcm[:hdrEnd+20], []*ecdh.PrivateKey{k}, ErrIntegrity},
		{"shorter than the tag", gcm[:hdrEnd+3], []*ecdh.PrivateKey{k}, ErrTruncated},
		{"flipped tag", flip(gcm, len(gcm)-1), []*ecdh.PrivateKey{k}, ErrIntegrity},
		{"flipped body", flip(gcm, hdrEnd+50), []*ecdh.PrivateKey{k}, ErrIntegrity},
		{"flipped HMAC", flip(cbc, len(cbc)-1), []*ecdh.PrivateKey{k}, ErrIntegrity},
		{"truncated CBC", cbc[:len(cbc)-40], []*ecdh.PrivateKey{k}, ErrIntegrity},
		{"unknown cipher", replace(gcm, gcmOID, ofb), []*ecdh.PrivateKey{k}, ErrUnsupported},
		{"wrong key", gcm, []*ecdh.PrivateKey{genKey(t, ecdh.P256())}, ErrNoKey},
		{"no key", gcm, nil, ErrNoKey},
		{"RSA slot", setByte(gcm, keySlot, keyTypeRSA), []*ecdh.PrivateKey{k}, ErrUnsupported},
		{"ML-KEM slot", setByte(gcm, keySlot, keyTypeKEM), []*ecdh.PrivateKey{k}, ErrUnsupported},
		{"HMAC flag with GCM", setByte(gcm, len(Magic)+4, flagHMAC), []*ecdh.PrivateKey{k}, ErrUnsupported},
		{"zero PBKDF2 rounds", setU32(gcm, rounds, 0), []*ecdh.PrivateKey{k}, ErrCorrupt},
		{"four billion PBKDF2 rounds", setU32(gcm, rounds, 0xffffffff), []*ecdh.PrivateKey{k}, ErrCorrupt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decryptAll(c.stream, c.keys)
			if err == nil {
				t.Fatalf("no error, %d bytes", len(got))
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("failure reported as EOF")
			}
		})
	}
}

func TestEmptyInputIsEmptyBody(t *testing.T) {
	got, err := decryptAll(nil, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %d bytes, err %v", len(got), err)
	}
}

func setU32(b []byte, i int, v uint32) []byte {
	c := append([]byte(nil), b...)
	binary.BigEndian.PutUint32(c[i:], v)
	return c
}

func setByte(b []byte, i int, v byte) []byte {
	c := append([]byte(nil), b...)
	c[i] = v
	return c
}

func decryptAll(stream []byte, keys []*ecdh.PrivateKey) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(stream), keys...)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestParsePrivateKeyRefusesX448AndMLKEM(t *testing.T) {
	cases := []struct {
		name string
		oid  asn1.ObjectIdentifier
	}{
		{"X448", oidX448},
		{"ML-KEM-768", append(append(asn1.ObjectIdentifier{}, oidMLKEM...), 2)},
	}
	for _, c := range cases {
		der, err := asn1.Marshal(struct {
			Version int
			Algo    pkix.AlgorithmIdentifier
			Key     []byte
		}{0, pkix.AlgorithmIdentifier{Algorithm: c.oid}, make([]byte, 56)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParsePrivateKeyPEM(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), strings.SplitN(c.name, "-", 2)[0]) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

func FuzzNewReader(f *testing.F) {
	k := genKey(f, ecdh.P256())
	f.Add(encodeV2(f, k.PublicKey(), true, []byte("hello")))
	f.Add(encodeV2(f, k.PublicKey(), false, []byte("hello")))
	f.Add(append(append([]byte(nil), Magic...), 1, 0, 4, 0, 1, 2, 0))
	f.Fuzz(func(t *testing.T, stream []byte) {
		got, err := decryptAll(stream, []*ecdh.PrivateKey{k})
		if err == nil && !IsEncrypted(stream) {
			t.Fatalf("plain input decrypted to %d bytes", len(got))
		}
	})
}

// TestReferenceVectors decrypts streams the reference wrote: every curve,
// cipher and integrity mode, several sizes, each checked by its SHA-256.
func TestReferenceVectors(t *testing.T) {
	dir := filepath.Join("testdata", "reference")
	mf, err := os.Open(filepath.Join(dir, "MANIFEST"))
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Close()
	sc := bufio.NewScanner(mf)
	n := 0
	for sc.Scan() {
		// stream key sha256-of-plaintext
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 {
			continue
		}
		n++
		keyPEM, err := os.ReadFile(filepath.Join(dir, fields[1]))
		if err != nil {
			t.Fatal(err)
		}
		key, err := ParsePrivateKeyPEM(keyPEM)
		if err != nil {
			t.Errorf("%s: %v", fields[1], err)
			continue
		}
		stream, err := os.ReadFile(filepath.Join(dir, fields[0]))
		if err != nil {
			t.Fatal(err)
		}
		got, err := decryptAll(stream, []*ecdh.PrivateKey{key})
		sum := sha256.Sum256(got)
		if err != nil || hex.EncodeToString(sum[:]) != fields[2] {
			t.Errorf("%s: err=%v sha256 match=%v", fields[0], err, hex.EncodeToString(sum[:]) == fields[2])
		}
	}
	if n == 0 {
		t.Fatal("MANIFEST lists no vectors")
	}
	t.Logf("%d reference vectors", n)
}

// TestServedDigests holds the stand's reference check to the vector: an IMAP
// server sends the plaintext with every bare LF as CRLF, so SERVED records
// that body's size, its SHA-256 and the SHA-256 of one partial range.
func TestServedDigests(t *testing.T) {
	dir := filepath.Join("testdata", "reference")
	raw, err := os.ReadFile(filepath.Join(dir, "SERVED"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(line)
		if len(f) != 7 {
			t.Fatalf("SERVED line %q: want 7 fields", line)
		}
		keyPEM, err := os.ReadFile(filepath.Join(dir, f[1]))
		if err != nil {
			t.Fatal(err)
		}
		key, err := ParsePrivateKeyPEM(keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := os.ReadFile(filepath.Join(dir, f[0]))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := decryptAll(stream, []*ecdh.PrivateKey{key})
		if err != nil {
			t.Fatal(err)
		}
		var served []byte
		for i, b := range plain {
			if b == '\n' && (i == 0 || plain[i-1] != '\r') {
				served = append(served, '\r')
			}
			served = append(served, b)
		}
		var off, n int
		if _, err := fmt.Sscan(f[4]+" "+f[5], &off, &n); err != nil {
			t.Fatal(err)
		}
		whole := sha256.Sum256(served)
		part := sha256.Sum256(served[off : off+n])
		got := fmt.Sprintf("%d %x %x", len(served), whole, part)
		if want := f[2] + " " + f[3] + " " + f[6]; got != want {
			t.Errorf("%s served: got %s, want %s", f[0], got, want)
		}
	}
}
