package agestream

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"filippo.io/age"
)

const chunk = 64 << 10

type keyPair struct {
	name string
	id   age.Identity
	rcpt age.Recipient
	// fixed is the overhead of a message up to one chunk, its tag included.
	fixed int
}

func keyPairs(t testing.TB) []keyPair {
	t.Helper()
	x, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	h, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return []keyPair{
		{"X25519", x, x.Recipient(), 200},
		{"mlkem768x25519", h, h.Recipient(), 1659},
	}
}

func seal(t testing.TB, r age.Recipient, plain []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := NewWriter(&b, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestRoundTrip(t *testing.T) {
	sizes := []int{0, 1, chunk, chunk + 1, 2 * chunk, 1 << 20}
	for _, k := range keyPairs(t) {
		for _, n := range sizes {
			plain := random(n)
			sealed := seal(t, k.rcpt, plain)
			if !IsEncrypted(sealed) {
				t.Fatalf("%s n=%d: no magic", k.name, n)
			}
			r, err := NewReader(iotest.OneByteReader(bytes.NewReader(sealed)), k.id)
			if err != nil {
				t.Fatalf("%s n=%d: %v", k.name, n, err)
			}
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("%s n=%d: err=%v equal=%v", k.name, n, err, bytes.Equal(got, plain))
			}
		}
	}
}

// TestOverhead pins the bytes age adds per message: the fixed part (header and
// the first chunk's tag) and 16 bytes for every further chunk.
func TestOverhead(t *testing.T) {
	cases := []struct{ n, extraChunks int }{{0, 0}, {1024, 0}, {chunk, 0}, {chunk + 1, 1}, {2 * chunk, 1}, {1 << 20, 15}}
	for _, k := range keyPairs(t) {
		for _, c := range cases {
			got := len(seal(t, k.rcpt, random(c.n))) - c.n
			if want := k.fixed + 16*c.extraChunks; got != want {
				t.Errorf("%s n=%d: overhead %d, want %d", k.name, c.n, got, want)
			}
		}
	}
}

func TestReaderAt(t *testing.T) {
	for _, k := range keyPairs(t) {
		plain := random(3*chunk + 100)
		sealed := seal(t, k.rcpt, plain)
		ra, n, err := NewReaderAt(bytes.NewReader(sealed), int64(len(sealed)), k.id)
		if err != nil || n != int64(len(plain)) {
			t.Fatalf("%s: n=%d err=%v", k.name, n, err)
		}
		reads := []struct{ off, n int }{
			{0, 10}, {chunk - 5, 10}, {chunk, 1}, {2*chunk - 1, 2}, {len(plain) - 7, 7}, {100, 2 * chunk},
		}
		for _, rd := range reads {
			buf := make([]byte, rd.n)
			if _, err := ra.ReadAt(buf, int64(rd.off)); err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("%s at %d: %v", k.name, rd.off, err)
			}
			if !bytes.Equal(buf, plain[rd.off:rd.off+rd.n]) {
				t.Fatalf("%s at %d len %d: wrong bytes", k.name, rd.off, rd.n)
			}
		}
	}
}

func TestRefused(t *testing.T) {
	k := keyPairs(t)[0]
	other := keyPairs(t)[0]
	plain := random(2*chunk + 10)
	sealed := seal(t, k.rcpt, plain)
	flip := func(i int) []byte { c := append([]byte(nil), sealed...); c[i] ^= 1; return c }

	cases := []struct {
		name   string
		stream []byte
		id     age.Identity
	}{
		{"truncated", sealed[:len(sealed)-100], k.id},
		{"cut at a chunk end", sealed[:len(sealed)-26], k.id},
		{"flipped last chunk", flip(len(sealed) - 5), k.id},
		{"flipped middle chunk", flip(len(sealed) - chunk), k.id},
		{"wrong key", sealed, other.id},
		{"plain text", plain, k.id},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := NewReader(bytes.NewReader(c.stream), c.id)
			if err == nil {
				var got []byte
				got, err = io.ReadAll(r)
				if err == nil {
					t.Fatalf("no error, %d bytes", len(got))
				}
			}
			if errors.Is(err, io.EOF) {
				t.Fatal("failure reported as EOF")
			}
			ra, _, err := NewReaderAt(bytes.NewReader(c.stream), int64(len(c.stream)), c.id)
			if err == nil {
				buf := make([]byte, len(plain))
				if _, err = ra.ReadAt(buf, 0); err == nil {
					t.Fatal("ReaderAt: no error")
				}
			}
		})
	}
}

func TestNoRecipient(t *testing.T) {
	if _, err := NewWriter(io.Discard); !errors.Is(err, ErrNoRecipient) {
		t.Fatalf("err %v", err)
	}
}

func FuzzReader(f *testing.F) {
	x, err := age.GenerateX25519Identity()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seal(f, x.Recipient(), []byte("hello")))
	f.Add(seal(f, x.Recipient(), random(chunk+3)))
	f.Fuzz(func(t *testing.T, b []byte) {
		if r, err := NewReader(bytes.NewReader(b), x); err == nil {
			_, _ = io.ReadAll(r)
		}
		if ra, n, err := NewReaderAt(bytes.NewReader(b), int64(len(b)), x); err == nil && n > 0 {
			_, _ = ra.ReadAt(make([]byte, min(n, 4096)), 0)
		}
	})
}
