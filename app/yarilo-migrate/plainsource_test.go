package main

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/agestream"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
	indexfile "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A source body the reference encrypted is decrypted before the save, which
// would refuse it as planted; a plaintext one passes as it is.
func TestPlainSource(t *testing.T) {
	dir := filepath.Join("..", "..", "internal", "mailcrypt", "refstream", "testdata", "reference")
	enc, err := os.ReadFile(filepath.Join(dir, "prime256v1-gcm-1000.bin"))
	if err != nil {
		t.Fatal(err)
	}
	pemKey, _ := os.ReadFile(filepath.Join(dir, "prime256v1.pem"))
	key, err := refstream.ParsePrivateKeyPEM(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	withKey := mailcrypt.New(mailcrypt.Options{ReferenceKeys: []*ecdh.PrivateKey{key}})

	got, err := plainSource(withKey, enc)
	if err != nil || len(got) != 1000 || mailcrypt.Classify(got) != mailcrypt.Plain {
		t.Fatalf("decrypted %d bytes, %v", len(got), err)
	}
	if err := withKey.Seal(&bytes.Buffer{}, bytes.NewReader(got)); err != nil {
		t.Fatalf("the decrypted body is refused on save: %v", err)
	}
	plain := []byte("From: a@b\r\n\r\nx\r\n")
	if got, err := plainSource(nil, plain); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("plaintext changed: %v", err)
	}
	if _, err := plainSource(nil, enc); !errors.Is(err, mailcrypt.ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
}

// TestImportOfAnEncryptedSource: a message the reference encrypted is
// imported as its plaintext and sealed in the destination, through the
// backend pickBackend builds; before both fixes it was refused as planted,
// and the destination ignored the configured encryption.
func TestImportOfAnEncryptedSource(t *testing.T) {
	dir := filepath.Join("..", "..", "internal", "mailcrypt", "refstream", "testdata", "reference")
	enc, err := os.ReadFile(filepath.Join(dir, "prime256v1-gcm-1000.bin"))
	if err != nil {
		t.Fatal(err)
	}
	pemKey, _ := os.ReadFile(filepath.Join(dir, "prime256v1.pem"))
	key, err := refstream.ParsePrivateKeyPEM(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	crypt := mailcrypt.New(mailcrypt.Options{
		Recipients: []age.Recipient{id.Recipient()}, Identities: []age.Identity{id},
		ReferenceKeys: []*ecdh.PrivateKey{key},
	})
	box, err := pickBackend("mdbox", crypt)
	if err != nil {
		t.Fatal(err)
	}
	src, dst := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "example.com", "alice"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolver := &mailbox.Resolver{Root: dst, HomeTemplate: "%d/%n"}
	w := stubWalker{msgs: []sourceMessage{{Folder: "INBOX", Body: enc}}}
	if _, _, err := migrateUser(w, src, box, indexfile.New(), resolver, importOpts{Driver: "mdbox", Crypt: crypt}, importUser); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var sealedFiles int
	_ = filepath.WalkDir(dst, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			if b, rerr := os.ReadFile(p); rerr == nil && bytes.Contains(b, agestream.Magic) {
				sealedFiles++
			}
		}
		return nil
	})
	if sealedFiles == 0 {
		t.Fatal("the destination holds no sealed body")
	}
}
