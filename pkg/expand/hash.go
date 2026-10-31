package expand

import (
	"crypto/md5"  //nolint:gosec // a template may ask for it
	"crypto/sha1" //nolint:gosec // a template may ask for it
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"encoding/binary"
	"hash"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/crypto/md4" //nolint:staticcheck // a template may ask for it
)

// hashMethods are the digests hash() takes by name, as the reference names
// them.
var hashMethods = map[string]func() hash.Hash{
	"md4":      md4.New,
	"md5":      md5.New,
	"sha1":     sha1.New,
	"sha256":   sha256.New,
	"sha384":   sha512.New384,
	"sha512":   sha512.New,
	"sha3-256": func() hash.Hash { return sha3.New256() },
	"sha3-512": func() hash.Hash { return sha3.New512() },
	"xxh64":    func() hash.Hash { return xxhash.New() },
	"size":     func() hash.Hash { return &sizeHash{} },
}

// sizeHash's digest is the input length, eight bytes big-endian.
type sizeHash struct{ n uint64 }

func (h *sizeHash) Write(p []byte) (int, error) { h.n += uint64(len(p)); return len(p), nil }
func (h *sizeHash) Sum(b []byte) []byte         { return binary.BigEndian.AppendUint64(b, h.n) }
func (h *sizeHash) Reset()                      { h.n = 0 }
func (h *sizeHash) Size() int                   { return 8 }
func (h *sizeHash) BlockSize() int              { return 1 }
