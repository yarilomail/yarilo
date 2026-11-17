package mailcrypt

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

// countingBody counts the bytes read from the stored body.
type countingBody struct {
	*bytes.Reader
	read atomic.Int64
}

func (c *countingBody) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.Reader.ReadAt(p, off)
	c.read.Add(int64(n))
	return n, err
}

func newBody(b []byte) *countingBody { return &countingBody{Reader: bytes.NewReader(b)} }

func sealed(t *testing.T, l *Layer, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := l.Seal(&out, bytes.NewReader(plain)); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func open(t *testing.T, l *Layer, stored []byte) []byte {
	t.Helper()
	o, err := l.Open(newBody(stored), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	b, err := io.ReadAll(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ageLayer(t *testing.T) (*Layer, *age.X25519Identity) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{Recipients: []age.Recipient{id.Recipient()}, Identities: []age.Identity{id}}), id
}

func TestSealOpen(t *testing.T) {
	enc, _ := ageLayer(t)
	cases := []struct {
		name    string
		l       *Layer
		wantAge bool
	}{{"off", New(Options{}), false}, {"nil", nil, false}, {"age", enc, true}}
	for _, c := range cases {
		for _, n := range []int{0, 1, 70000} {
			plain := make([]byte, n)
			_, _ = rand.Read(plain)
			stored := sealed(t, c.l, plain)
			if got := Classify(stored) == Age; got != c.wantAge && n > 0 {
				t.Fatalf("%s n=%d: age on disk %v", c.name, n, got)
			}
			if got := open(t, c.l, stored); !bytes.Equal(got, plain) {
				t.Fatalf("%s n=%d: plaintext differs", c.name, n)
			}
			size, err := c.l.PlainSize(newBody(stored))
			if err != nil || size != int64(n) {
				t.Fatalf("%s n=%d: PlainSize %d %v", c.name, n, size, err)
			}
		}
	}
}

func TestPlantedRefusedInBothModes(t *testing.T) {
	enc, _ := ageLayer(t)
	ageBody := sealed(t, enc, []byte("hello"))
	ref := append(append([]byte(nil), refstream.Magic...), 2, 0, 0, 0)
	for _, l := range []*Layer{New(Options{}), enc} {
		for _, body := range [][]byte{ageBody, ref} {
			if err := l.Seal(io.Discard, bytes.NewReader(body)); !errors.Is(err, ErrPlanted) {
				t.Fatalf("writes=%v: %v", l.Writes(), err)
			}
		}
		// A body that only mentions the signature further in is plain mail.
		if err := l.Seal(io.Discard, strings.NewReader("Subject: x\r\n\r\nage-encryption.org/v1\n")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRotation(t *testing.T) {
	oldID, _ := age.GenerateX25519Identity()
	newID, _ := age.GenerateHybridIdentity()
	before := New(Options{Recipients: []age.Recipient{oldID.Recipient()}})
	after := New(Options{Recipients: []age.Recipient{newID.Recipient()}, Identities: []age.Identity{oldID, newID}})
	oldMail := sealed(t, before, []byte("old"))
	newMail := sealed(t, after, []byte("new"))
	if string(open(t, after, oldMail)) != "old" || string(open(t, after, newMail)) != "new" {
		t.Fatal("rotation lost a message")
	}
	onlyOld := New(Options{Identities: []age.Identity{oldID}})
	if _, err := onlyOld.Open(newBody(newMail), nil); err == nil {
		t.Fatal("new mail opened with the old key alone")
	}
}

func TestWrongKey(t *testing.T) {
	enc, _ := ageLayer(t)
	other, _ := ageLayer(t)
	if _, err := other.Open(newBody(sealed(t, enc, []byte("x"))), nil); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err %v", err)
	}
}

// TestPartialReadTouchesOnlyItsChunks: a 4 KiB read from the middle of a
// 10 MiB age body reads two chunks and the header, not the body.
func TestPartialReadTouchesOnlyItsChunks(t *testing.T) {
	enc, _ := ageLayer(t)
	plain := make([]byte, 10<<20)
	_, _ = rand.Read(plain)
	body := newBody(sealed(t, enc, plain))
	o, err := enc.Open(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := body.read.Load()
	buf := make([]byte, 4096)
	if _, err := o.ReadAt(buf, 5<<20); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, plain[5<<20:5<<20+4096]) {
		t.Fatal("wrong bytes")
	}
	if read := body.read.Load() - before; read > 3*(64<<10+16) {
		t.Fatalf("read %d bytes of the file for 4 KiB", read)
	}
	if o.Size() != int64(len(plain)) {
		t.Fatalf("size %d", o.Size())
	}
}

func referenceVector(t *testing.T) (stream []byte, key *ecdh.PrivateKey, plainSHA string) {
	t.Helper()
	dir := filepath.Join("refstream", "testdata", "reference")
	f, err := os.Open(filepath.Join(dir, "MANIFEST"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) == 3 && strings.HasSuffix(fs[0], "-gcm-1000.bin") && strings.HasPrefix(fs[0], "prime256v1") {
			stream, _ = os.ReadFile(filepath.Join(dir, fs[0]))
			pemKey, _ := os.ReadFile(filepath.Join(dir, fs[1]))
			key, err = refstream.ParsePrivateKeyPEM(pemKey)
			if err != nil {
				t.Fatal(err)
			}
			return stream, key, fs[2]
		}
	}
	t.Fatal("no vector")
	return nil, nil, ""
}

func TestReferenceThroughCache(t *testing.T) {
	stream, key, _ := referenceVector(t)
	dir := t.TempDir()
	foreign := filepath.Join(dir, "yarilo-pop3.1", "body-1")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, err := NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("another process's cache was swept: %v", err)
	}
	now := time.Unix(1000, 0)
	cache.mu.Lock()
	cache.now = func() time.Time { return now }
	cache.mu.Unlock()
	l := New(Options{ReferenceKeys: []*ecdh.PrivateKey{key}, Cache: cache})

	plain := open(t, l, stream)
	if len(plain) != 1000 {
		t.Fatalf("plain %d bytes", len(plain))
	}
	files, _ := os.ReadDir(cache.dir)
	if len(files) != 1 {
		t.Fatalf("cache files %d", len(files))
	}
	info, _ := files[0].Info()
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	if !bytes.Equal(open(t, l, stream), plain) {
		t.Fatal("second open differs")
	}
	cache.mu.Lock()
	now = now.Add(cacheTTL + time.Second)
	cache.mu.Unlock()
	if cache.Used() != 0 {
		t.Fatalf("used %d after expiry", cache.Used())
	}
	if files, _ := os.ReadDir(cache.dir); len(files) != 0 {
		t.Fatalf("expired file kept: %v", files)
	}
}

func TestReferenceBypassPastBudget(t *testing.T) {
	stream, key, _ := referenceVector(t)
	cache, err := NewCache(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var bypassed int
	cache.Bypassed = func() { bypassed++ }
	l := New(Options{ReferenceKeys: []*ecdh.PrivateKey{key}, Cache: cache})
	o, err := l.Open(newBody(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	got, err := io.ReadAll(o)
	if err != nil || len(got) != 1000 || o.Size() != 1000 {
		t.Fatalf("stream: %d bytes, size %d, %v", len(got), o.Size(), err)
	}
	buf := make([]byte, 10)
	if _, err := o.ReadAt(buf, 500); err != nil || !bytes.Equal(buf, got[500:510]) {
		t.Fatalf("ReadAt backwards: %v", err)
	}
	if files, _ := os.ReadDir(cache.dir); len(files) != 0 || bypassed != 1 {
		t.Fatalf("files %d, bypassed %d", len(files), bypassed)
	}
}

// An earlier process of the same binary is gone: its plaintext is removed.
func TestCacheSweepsADeadPredecessor(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, procName+".999999")
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCache(dir, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("a dead process's cache kept: %v", err)
	}
}

// A cached file that vanished, and two sessions filling the same body at once,
// leave the budget at one body and one file.
func TestCacheBudgetDoesNotLeak(t *testing.T) {
	stream, key, _ := referenceVector(t)
	cache, err := NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	l := New(Options{ReferenceKeys: []*ecdh.PrivateKey{key}, Cache: cache})
	open(t, l, stream)
	files, _ := os.ReadDir(cache.dir)
	for _, f := range files {
		_ = os.Remove(filepath.Join(cache.dir, f.Name()))
	}
	open(t, l, stream)
	if used := cache.Used(); used != 1000 {
		t.Fatalf("after a vanished file: used %d", used)
	}

	cache2, err := NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	l2 := New(Options{ReferenceKeys: []*ecdh.PrivateKey{key}, Cache: cache2})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := l2.Open(newBody(stream), nil)
			if err == nil {
				_, _ = io.ReadAll(o)
				_ = o.Close()
			}
		}()
	}
	wg.Wait()
	if used := cache2.Used(); used != 1000 {
		t.Fatalf("after concurrent fills: used %d", used)
	}
	if files, _ := os.ReadDir(cache2.dir); len(files) != 1 {
		t.Fatalf("after concurrent fills: %d files", len(files))
	}
}

// An idle cache removes the plaintext on time, with no read to trigger it.
func TestCacheExpiresWhenIdle(t *testing.T) {
	stream, key, _ := referenceVector(t)
	cache, err := newCache(t.TempDir(), 1<<20, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	l := New(Options{ReferenceKeys: []*ecdh.PrivateKey{key}, Cache: cache})
	open(t, l, stream)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if files, _ := os.ReadDir(cache.dir); len(files) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("idle plaintext outlived its time")
}

func TestReferenceWithoutKey(t *testing.T) {
	stream, _, _ := referenceVector(t)
	if _, err := New(Options{}).Open(newBody(stream), nil); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err %v", err)
	}
}
