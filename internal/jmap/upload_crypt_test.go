package jmap

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
)

// TestSealedUpload: with the layer writing, a blob is ciphertext on disk
// under <id>.age, opens as its plaintext, keeps the plaintext's id and size
// for the client and the quota, and expires on its age with no key at hand.
func TestSealedUpload(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	crypt := mailcrypt.New(mailcrypt.Options{Recipients: []age.Recipient{id.Recipient()}, Identities: []age.Identity{id}})
	dir := t.TempDir()
	st := &uploadStore{dir: dir, user: "u", ttl: time.Hour, crypt: crypt}
	blob := []byte(strings.Repeat("attachment bytes nobody else may read\n", 200))

	up, err := st.put(context.Background(), bytes.NewReader(blob), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if up.size != int64(len(blob)) {
		t.Fatalf("id size %d, plaintext %d", up.size, len(blob))
	}
	stored, err := os.ReadFile(filepath.Join(dir, up.String()+sealedSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("nobody else may read")) || mailcrypt.Classify(stored) != mailcrypt.Age {
		t.Fatal("blob not sealed on disk")
	}
	rc, err := st.open(context.Background(), up)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close() //nolint:errcheck
	if !bytes.Equal(got, blob) {
		t.Fatal("blob differs")
	}
	if p := st.pending(); p != int64(len(blob)) {
		t.Fatalf("pending %d, plaintext %d", p, len(blob))
	}

	// An age file is a valid upload too: sealed again, not refused.
	ageFile := []byte("age-encryption.org/v1\n-> X25519 whatever\n")
	if _, err := st.put(context.Background(), bytes.NewReader(ageFile), 1<<20); err != nil {
		t.Fatalf("an age file as an upload: %v", err)
	}

	keyless := &uploadStore{dir: dir, user: "u", ttl: time.Nanosecond}
	time.Sleep(time.Millisecond)
	keyless.sweepLocked()
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("expired sealed blobs kept without a key: %d", len(left))
	}
}
