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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

// cacheTTL is how long a decrypted reference body stays, as the reference keeps it.
const cacheTTL = 60 * time.Second

var errBypass = errors.New("mailcrypt: past the cache budget")

// procName and procID name this process's subdirectory: every container of a
// pod shares /tmp, and a session binary may run as several processes.
var (
	procName = filepath.Base(os.Args[0])
	procID   = os.Getpid()
)

// Cache keeps decrypted reference-format bodies in temporary files under this
// process's own subdirectory, bounded in total bytes, each removed by a timer
// cacheTTL after its last use. The files hold plaintext: the directory must
// not be on the mail volume (storage.mail_temp_dir).
type Cache struct {
	dir   string
	limit int64
	ttl   time.Duration

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

// NewCache returns a cache of at most limit bytes in a subdirectory of dir
// that only this process uses; limit 0 means no cache. It removes what an
// earlier process of the same binary left, and nobody else's files.
func NewCache(dir string, limit int64) (*Cache, error) {
	return newCache(dir, limit, cacheTTL)
}

func newCache(dir string, limit int64, ttl time.Duration) (*Cache, error) {
	if limit <= 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mailcrypt/cache: %w", err)
	}
	sweepStale(dir)
	own := filepath.Join(dir, fmt.Sprintf("%s.%d", procName, procID))
	if err := os.RemoveAll(own); err != nil {
		return nil, fmt.Errorf("mailcrypt/cache: %w", err)
	}
	if err := os.Mkdir(own, 0o700); err != nil {
		return nil, fmt.Errorf("mailcrypt/cache: %w", err)
	}
	c := &Cache{dir: own, limit: limit, ttl: ttl, entries: map[string]*cacheEntry{}, now: time.Now}
	go c.janitor()
	return c, nil
}

// sweepStale removes the subdirectories of this binary whose process is gone:
// a restarted container leaves its predecessor's plaintext behind.
func sweepStale(dir string) {
	old, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range old {
		pidStr, ok := strings.CutPrefix(e.Name(), procName+".")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid == procID || processAlive(pid) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// janitor expires entries on time, used or not: idle, the plaintext would
// otherwise stay until the next read.
func (c *Cache) janitor() {
	tick := max(c.ttl/4, 10*time.Millisecond)
	for range time.Tick(tick) {
		c.mu.Lock()
		c.expireLocked()
		c.mu.Unlock()
	}
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
	if o, ok := c.hitLocked(key); ok {
		c.mu.Unlock()
		return o, nil
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
	c.used -= body.Size()
	if err != nil {
		return nil, err
	}
	// Another session filled it meanwhile: keep theirs, drop ours.
	if o, ok := c.hitLocked(key); ok {
		_ = os.Remove(path)
		return o, nil
	}
	o, err := openFile(path, size)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	c.used += size
	c.entries[key] = &cacheEntry{path: path, size: size, expires: c.now().Add(c.ttl)}
	return o, nil
}

// hitLocked opens a cached entry; one whose file is gone is forgotten and its
// bytes returned to the budget.
func (c *Cache) hitLocked(key string) (Opened, bool) {
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	o, err := openFile(e.path, e.size)
	if err != nil {
		c.used -= e.size
		delete(c.entries, key)
		return nil, false
	}
	e.expires = c.now().Add(c.ttl)
	return o, true
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
