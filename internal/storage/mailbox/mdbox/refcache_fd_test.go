package mdbox

import (
	"crypto/ecdh"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

// Closing a reference-format record read through the cache closes the
// cache's file as well as the m.<N> file.
func TestReferenceRecordThroughTheCacheLeaksNoFile(t *testing.T) {
	vec := filepath.Join("..", "..", "..", "mailcrypt", "refstream", "testdata", "reference")
	body, err := os.ReadFile(filepath.Join(vec, "prime256v1-gcm-1000.bin"))
	if err != nil {
		t.Fatal(err)
	}
	pemKey, _ := os.ReadFile(filepath.Join(vec, "prime256v1.pem"))
	key, err := refstream.ParsePrivateKeyPEM(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := mailcrypt.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	layer := mailcrypt.New(mailcrypt.Options{ReferenceKeys: []*ecdh.PrivateKey{key}, Cache: cache})
	path := filepath.Join(t.TempDir(), "m.1")
	w, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	off, err := appendRecordToFile(w, body, parsedTrailer{guid: randomGUID(), vsize: 1000})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	count := func() int {
		fds, err := os.ReadDir("/dev/fd")
		if err != nil {
			t.Skip("no /dev/fd")
		}
		return len(fds)
	}
	before := count()
	for range 50 {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rc, err := openRecordBody(layer, f, off, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		if len(got) < 1000 {
			t.Fatalf("read %d bytes", len(got))
		}
		_ = rc.Close()
	}
	if after := count(); after > before+2 {
		t.Fatalf("open files %d -> %d over 50 reads", before, after)
	}
}
