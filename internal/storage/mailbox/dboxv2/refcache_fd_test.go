package dboxv2

import (
	"bytes"
	"crypto/ecdh"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

func openFDs(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("no /dev/fd")
	}
	return len(fds)
}

// Closing a reference-format body read through the cache closes the cache's
// file too: a CRLF filter between the two used to hide it.
func TestReferenceBodyThroughTheCacheLeaksNoFile(t *testing.T) {
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

	var file bytes.Buffer
	file.Write(encodeFileHeaderLine(1))
	file.Write(encodeMessageHeader(messageHeader{Size: uint64(len(body))}))
	file.Write(body)
	file.Write(encodeMetadataBlock([]metadataEntry{{Key: metaKeyVirtualSize, Value: "3e8"}}))
	path := filepath.Join(t.TempDir(), "u.1")
	if err := os.WriteFile(path, file.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	u := &userMailbox{b: New(WithCrypt(layer))}

	before := openFDs(t)
	for range 50 {
		rc, err := u.fetchPath(path)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		if len(got) < 1000 {
			t.Fatalf("read %d bytes", len(got))
		}
		_ = rc.Close()
	}
	if after := openFDs(t); after > before+2 {
		t.Fatalf("open files %d -> %d over 50 reads", before, after)
	}
}
