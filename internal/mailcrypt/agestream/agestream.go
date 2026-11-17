// Package agestream writes and reads message bodies in age
// (age-encryption.org/v1): ChaCha20-Poly1305 in 64 KiB chunks, each with its
// own tag, so a read at an offset decrypts only the chunks it touches.
package agestream

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"filippo.io/age"
)

// Magic opens every age file.
var Magic = []byte("age-encryption.org/v1\n")

// ErrNoRecipient refuses a write that nobody could read.
var ErrNoRecipient = errors.New("agestream: no recipient")

// IsEncrypted reports whether data starts with the age header line.
func IsEncrypted(prefix []byte) bool { return bytes.HasPrefix(prefix, Magic) }

// NewWriter encrypts to every recipient: a global key, or a folder key, of
// X25519 or the hybrid mlkem768x25519 type. Close writes the last chunk.
func NewWriter(dst io.Writer, recipients ...age.Recipient) (io.WriteCloser, error) {
	if len(recipients) == 0 {
		return nil, ErrNoRecipient
	}
	w, err := age.Encrypt(dst, recipients...)
	if err != nil {
		return nil, fmt.Errorf("agestream/write: %w", err)
	}
	return w, nil
}

// NewReader decrypts sequentially, for delivery and re-encryption.
func NewReader(src io.Reader, ids ...age.Identity) (io.Reader, error) {
	r, err := age.Decrypt(src, ids...)
	if err != nil {
		return nil, fmt.Errorf("agestream/read: %w", err)
	}
	return r, nil
}

// NewReaderAt reads the plaintext at any offset; size is the file's size and
// the returned int64 the plaintext's.
func NewReaderAt(src io.ReaderAt, size int64, ids ...age.Identity) (io.ReaderAt, int64, error) {
	r, n, err := age.DecryptReaderAt(src, size, ids...)
	if err != nil {
		return nil, 0, fmt.Errorf("agestream/read: %w", err)
	}
	return r, n, nil
}
