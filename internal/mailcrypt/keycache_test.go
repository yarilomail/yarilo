package mailcrypt

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// countingIdentity counts the unwraps that reach the private key.
type countingIdentity struct {
	age.Identity
	n atomic.Int32
}

func (c *countingIdentity) Unwrap(s []*age.Stanza) ([]byte, error) {
	c.n.Add(1)
	return c.Identity.Unwrap(s)
}

func hybridLayer(t *testing.T) (*Layer, *countingIdentity, *keyCache) {
	t.Helper()
	id, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ci := &countingIdentity{Identity: id}
	l := New(Options{Recipients: []age.Recipient{id.Recipient()}, Identities: []age.Identity{ci}})
	return l, ci, l.o.Identities[0].(*keyCache)
}

// TestKeyCacheUnwrapsABodyOnce: the second open of a body takes its key from
// the cache, a second body unwraps its own, a ForDriver copy shares the cache.
func TestKeyCacheUnwrapsABodyOnce(t *testing.T) {
	l, ci, _ := hybridLayer(t)
	a, b := sealed(t, l, []byte("first body\r\n")), sealed(t, l, []byte("second body\r\n"))
	hits := testutil.ToFloat64(metricKeyCache.WithLabelValues("hit"))
	misses := testutil.ToFloat64(metricKeyCache.WithLabelValues("miss"))
	for _, step := range []struct {
		l     *Layer
		body  []byte
		plain string
		calls int32
	}{
		{l, a, "first body\r\n", 1},
		{l, a, "first body\r\n", 1},
		{l.ForDriver("mdbox"), a, "first body\r\n", 1},
		{l, b, "second body\r\n", 2},
	} {
		if got := open(t, step.l, step.body); string(got) != step.plain {
			t.Fatalf("plaintext %q, want %q", got, step.plain)
		}
		if got := ci.n.Load(); got != step.calls {
			t.Fatalf("unwraps with the private key %d, want %d", got, step.calls)
		}
	}
	if d := testutil.ToFloat64(metricKeyCache.WithLabelValues("hit")) - hits; d != 2 {
		t.Errorf("hits +%v, want +2", d)
	}
	if d := testutil.ToFloat64(metricKeyCache.WithLabelValues("miss")) - misses; d != 2 {
		t.Errorf("misses +%v, want +2", d)
	}
}

// TestKeyCacheKeepsTheHeaderMACCheck: a header whose MAC was changed opens
// with no cached key and with one alike: it does not.
func TestKeyCacheKeepsTheHeaderMACCheck(t *testing.T) {
	l, _, _ := hybridLayer(t)
	good := sealed(t, l, []byte("body\r\n"))
	bad := bytes.Clone(good)
	i := bytes.Index(bad, []byte("\n--- ")) + 5
	bad[i] ^= 'A' ^ 'B'
	if bad[i] == good[i] {
		t.Fatal("MAC byte unchanged")
	}
	if _, err := l.Open(newBody(bad), nil); err == nil {
		t.Fatal("a changed MAC opened with no cached key")
	}
	open(t, l, good)
	if _, err := l.Open(newBody(bad), nil); err == nil {
		t.Fatal("a changed MAC opened with its key cached")
	}
}

// TestKeyCacheExpiresAndEvicts: an entry lives cacheTTL from its unwrap, and
// a full cache drops its oldest entry for a new one.
func TestKeyCacheExpiresAndEvicts(t *testing.T) {
	l, ci, kc := hybridLayer(t)
	now := time.Unix(1_000_000, 0)
	kc.now = func() time.Time { return now }
	kc.limit = 2
	a, b, c := sealed(t, l, []byte("a")), sealed(t, l, []byte("b")), sealed(t, l, []byte("c"))
	want := int32(0)
	step := func(body []byte, unwraps bool) {
		t.Helper()
		open(t, l, body)
		if unwraps {
			want++
		}
		if got := ci.n.Load(); got != want {
			t.Fatalf("unwraps %d, want %d", got, want)
		}
	}
	step(a, true)
	now = now.Add(cacheTTL - time.Second)
	step(a, false)
	now = now.Add(time.Second)
	step(a, true) // expired
	step(b, true)
	step(c, true) // full: a, the oldest, goes
	step(b, false)
	step(c, false)
	step(a, true)
	if len(kc.entries) != 2 || len(kc.order) != 2 {
		t.Fatalf("entries %d, order %d, want 2 and 2", len(kc.entries), len(kc.order))
	}
}

// TestKeyCacheRotationAndWrongKey: during a rotation both identities open
// through one cache; a body for neither is still ErrNoKey.
func TestKeyCacheRotationAndWrongKey(t *testing.T) {
	oldID, _ := age.GenerateX25519Identity()
	newID, _ := age.GenerateHybridIdentity()
	oldL := New(Options{Recipients: []age.Recipient{oldID.Recipient()}})
	both := New(Options{Identities: []age.Identity{newID, oldID}})
	if got := open(t, both, sealed(t, oldL, []byte("old"))); string(got) != "old" {
		t.Fatalf("old body %q", got)
	}
	other, _ := ageLayer(t)
	if _, err := both.Open(newBody(sealed(t, other, []byte("x"))), nil); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err %v, want ErrNoKey", err)
	}
}

// TestStanzaKeyCoversEveryField: type, each argument, the body and the
// boundaries between them all change the key.
func TestStanzaKeyCoversEveryField(t *testing.T) {
	base := func() []*age.Stanza {
		return []*age.Stanza{{Type: "X25519", Args: []string{"ab", "c"}, Body: []byte("body")}}
	}
	seen := map[[32]byte]string{stanzaKey(base()): "base"}
	for name, change := range map[string]func(s []*age.Stanza) []*age.Stanza{
		"type":       func(s []*age.Stanza) []*age.Stanza { s[0].Type = "X25519x"; return s },
		"arg":        func(s []*age.Stanza) []*age.Stanza { s[0].Args[1] = "d"; return s },
		"arg split":  func(s []*age.Stanza) []*age.Stanza { s[0].Args = []string{"a", "bc"}; return s },
		"arg count":  func(s []*age.Stanza) []*age.Stanza { s[0].Args = []string{"abc"}; return s },
		"body":       func(s []*age.Stanza) []*age.Stanza { s[0].Body = []byte("bodz"); return s },
		"second":     func(s []*age.Stanza) []*age.Stanza { return append(s, &age.Stanza{Type: "X25519"}) },
		"type split": func(s []*age.Stanza) []*age.Stanza { s[0].Type = "X2551"; s[0].Args[0] = "9ab"; return s },
	} {
		k := stanzaKey(change(base()))
		if prev, dup := seen[k]; dup {
			t.Errorf("%s gives the key of %s", name, prev)
		}
		seen[k] = name
	}
}
