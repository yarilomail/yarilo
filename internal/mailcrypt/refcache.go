package mailcrypt

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

// cacheTTL is how long a decrypted reference body stays, as the reference keeps it.
const cacheTTL = 60 * time.Second

var errBypass = errors.New("mailcrypt: past the cache budget")

// Cache keeps decrypted reference-format bodies in temporary files under one
// directory, bounded in total bytes. The files hold plaintext: the directory
// must not be on the mail volume (storage.mail_temp_dir).
type Cache struct {
	dir   string
	limit int64

	mu      sync.Mutex
	used    int64
	entries map[string]*cacheEntry
	now     func() time.Time
	// Bypassed counts bodies read as a stream because the budget was full.
	Bypassed func()
}

type cacheEntry struct {
	path    string
	size    int64
	expires time.Time
}

// NewCache empties dir and returns a cache of at most limit bytes; limit 0
// means no cache.
func NewCache(dir string, limit int64) (*Cache, error) {
	if limit <= 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mailcrypt/cache: %w", err)
	}
	old, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("mailcrypt/cache: %w", err)
	}
	for _, e := range old {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	return &Cache{dir: dir, limit: limit, entries: map[string]*cacheEntry{}, now: time.Now}, nil
}

// Used is the bytes the cache holds now.
func (c *Cache) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked()
	return c.used
}

func cacheKey(body Body) (string, error) {
	head := make([]byte, 4096)
	n, err := body.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d:", body.Size())
	h.Write(head[:n])
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *Cache) open(body Body, keys []*ecdh.PrivateKey) (Opened, error) {
	key, err := cacheKey(body)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.expireLocked()
	if e, ok := c.entries[key]; ok {
		e.expires = c.now().Add(cacheTTL)
		path, size := e.path, e.size
		c.mu.Unlock()
		if o, err := openFile(path, size); err == nil {
			return o, nil
		}
		c.mu.Lock()
	}
	// The plaintext is a little shorter than the ciphertext; reserve the latter.
	if c.used+body.Size() > c.limit {
		c.mu.Unlock()
		if c.Bypassed != nil {
			c.Bypassed()
		}
		return nil, errBypass
	}
	c.used += body.Size()
	c.mu.Unlock()

	size, path, err := c.fill(body, keys)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.used -= body.Size()
		return nil, err
	}
	c.used += size - body.Size()
	c.entries[key] = &cacheEntry{path: path, size: size, expires: c.now().Add(cacheTTL)}
	return openFile(path, size)
}

func (c *Cache) fill(body Body, keys []*ecdh.PrivateKey) (int64, string, error) {
	r, err := refstream.NewReader(io.NewSectionReader(body, 0, body.Size()), keys...)
	if err != nil {
		return 0, "", err
	}
	f, err := os.CreateTemp(c.dir, "body-*")
	if err != nil {
		return 0, "", fmt.Errorf("mailcrypt/cache: %w", err)
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return 0, "", err
	}
	return n, f.Name(), nil
}

// expireLocked unlinks entries past their time; open readers keep their
// file until they close it.
func (c *Cache) expireLocked() {
	now := c.now()
	for k, e := range c.entries {
		if now.After(e.expires) {
			_ = os.Remove(e.path)
			c.used -= e.size
			delete(c.entries, k)
		}
	}
}

func openFile(path string, size int64) (Opened, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return newOpened(f, size, f), nil
}

// refStream reads a reference body without a cache: forward as a stream, a
// backwards ReadAt decrypts again from the start, as the reference does.
type refStream struct {
	body   Body
	keys   []*ecdh.PrivateKey
	closer io.Closer
	r      io.Reader
	pos    int64
	size   int64 // -1 until known
}

func newRefStream(body Body, keys []*ecdh.PrivateKey, closer io.Closer) (*refStream, error) {
	s := &refStream{body: body, keys: keys, closer: closer, size: -1}
	if err := s.rewind(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *refStream) rewind() error {
	r, err := refstream.NewReader(io.NewSectionReader(s.body, 0, s.body.Size()), s.keys...)
	if err != nil {
		return err
	}
	s.r, s.pos = r, 0
	return nil
}

func (s *refStream) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.pos += int64(n)
	if errors.Is(err, io.EOF) && s.size < 0 {
		s.size = s.pos
	}
	return n, err
}

func (s *refStream) ReadAt(p []byte, off int64) (int, error) {
	if off < s.pos {
		if err := s.rewind(); err != nil {
			return 0, err
		}
	}
	if _, err := io.CopyN(io.Discard, s, off-s.pos); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(s, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}

func (s *refStream) Size() int64 {
	if s.size >= 0 {
		return s.size
	}
	r, err := refstream.NewReader(io.NewSectionReader(s.body, 0, s.body.Size()), s.keys...)
	if err != nil {
		return 0
	}
	n, _ := io.Copy(io.Discard, r)
	s.size = n
	return n
}

func (s *refStream) Close() error { return closeWith(s.closer, nil) }
