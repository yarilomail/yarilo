package mailcrypt

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"time"

	"filippo.io/age"
)

// keyCacheSize bounds the unwrapped file keys a process holds: about 120
// bytes each, so some 8 MB when full.
const keyCacheSize = 65536

// keyCache is an age identity that remembers each file key it unwraps, by
// the SHA-256 of the recipient stanzas, for cacheTTL: the hybrid unwrap is
// the cost of opening a small body, and one body is opened many times. Only
// keys that unwrapped are held; age checks the header MAC after Unwrap, so a
// cached key opens nothing a fresh unwrap would refuse.
type keyCache struct {
	ids   []age.Identity
	ttl   time.Duration
	limit int
	now   func() time.Time

	mu      sync.Mutex
	entries map[[32]byte]keyEntry
	order   [][32]byte // insertion order, which is expiry order
}

type keyEntry struct {
	key     []byte
	expires time.Time
}

func newKeyCache(ids []age.Identity, limit int, ttl time.Duration) *keyCache {
	return &keyCache{ids: ids, ttl: ttl, limit: limit, now: time.Now, entries: map[[32]byte]keyEntry{}}
}

// Unwrap returns the cached file key of stanzas, or the first identity's
// that unwraps them.
func (c *keyCache) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	id := stanzaKey(stanzas)
	if key, ok := c.get(id); ok {
		metricKeyCache.WithLabelValues("hit").Inc()
		return key, nil
	}
	metricKeyCache.WithLabelValues("miss").Inc()
	for _, i := range c.ids {
		key, err := i.Unwrap(stanzas)
		if errors.Is(err, age.ErrIncorrectIdentity) {
			continue
		}
		if err != nil {
			return nil, err
		}
		c.put(id, key)
		return key, nil
	}
	return nil, age.ErrIncorrectIdentity
}

func (c *keyCache) get(id [32]byte) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok || !c.now().Before(e.expires) {
		return nil, false
	}
	return slices.Clone(e.key), true
}

func (c *keyCache) put(id [32]byte, key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if _, ok := c.entries[id]; ok {
		return
	}
	for len(c.order) > 0 {
		e := c.entries[c.order[0]]
		if len(c.entries) < c.limit && now.Before(e.expires) {
			break
		}
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.entries[id] = keyEntry{key: slices.Clone(key), expires: now.Add(c.ttl)}
	c.order = append(c.order, id)
}

// stanzaKey digests every field of every stanza, each length-prefixed.
func stanzaKey(stanzas []*age.Stanza) [32]byte {
	h := sha256.New()
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	for _, s := range stanzas {
		field([]byte(s.Type))
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s.Args)))
		h.Write(n[:])
		for _, a := range s.Args {
			field([]byte(a))
		}
		field(s.Body)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}
