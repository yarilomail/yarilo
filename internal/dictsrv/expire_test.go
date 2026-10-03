package dictsrv

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/proxy"
	_ "github.com/yarilomail/yarilo/pkg/dict/redis"
)

// serveRedis runs the service over a redis dict on miniredis and returns a
// proxy client, the service address and the Redis under it.
func serveRedis(t *testing.T) (dict.Dict, string, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	real, err := dict.Open(dict.Config{Driver: "redis", Settings: map[string]any{"addr": mr.Addr(), "prefix": "t:"}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go New(map[string]dict.Dict{"d": real}, nil).Serve(ctx, ln) //nolint:errcheck
	c := proxy.New(ln.Addr().String(), "d", nil)
	t.Cleanup(func() { _ = c.Close() })
	return c, ln.Addr().String(), mr
}

// onlyKey is the one key under prefix, so a row reads the TTL of what it wrote.
func onlyKey(t *testing.T, mr *miniredis.Miniredis, contains string) string {
	t.Helper()
	for _, k := range mr.Keys() {
		if strings.Contains(k, contains) {
			return k
		}
	}
	t.Fatalf("no key containing %q in %v", contains, mr.Keys())
	return ""
}

// A transaction's TTL crosses the proxy in BEGIN, as the dict-client protocol
// carries it; the duplicate test and vacation markers depend on it.
func TestTheProxyCarriesTheTTL(t *testing.T) {
	ctx := context.Background()
	user := "u2@d.test"

	t.Run("BEGIN with expire secs", func(t *testing.T) {
		c, _, mr := serveRedis(t)
		tx, err := c.Begin(ctx, &dict.OpSettings{Username: user, ExpireSecs: 600})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Set("priv/ttl", []byte("1")); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := mr.TTL(onlyKey(t, mr, "priv/ttl")); got != 600*time.Second {
			t.Fatalf("TTL %v, want 10m", got)
		}
	})

	t.Run("duplicate test through the service", func(t *testing.T) {
		c, _, mr := serveRedis(t)
		dup := sieve.NewDictDuplicateTracker(c, user)
		if seen, err := dup.IsDuplicate(ctx, "", "<m@x>", 3600, false); err != nil || seen {
			t.Fatalf("first: seen %v err %v", seen, err)
		}
		if got := mr.TTL(onlyKey(t, mr, "/sieve/duplicate/")); got != time.Hour {
			t.Fatalf("duplicate key TTL %v, want the period, 1h", got)
		}
		if seen, _ := dup.IsDuplicate(ctx, "", "<m@x>", 3600, false); !seen {
			t.Fatal("second delivery not seen as duplicate")
		}
	})

	t.Run("vacation marker through the service", func(t *testing.T) {
		c, _, mr := serveRedis(t)
		store := sieve.NewScriptStore("dict", "", nil, c)
		if err := store.MarkVacationSent(ctx, user, "", "h", "s@x", 7*86400); err != nil {
			t.Fatal(err)
		}
		if got := mr.TTL(onlyKey(t, mr, "/sieve/vacation/")); got != 7*24*time.Hour {
			t.Fatalf("vacation marker TTL %v, want 7d", got)
		}
	})

	t.Run("a client without the expire field writes no TTL", func(t *testing.T) {
		_, addr, mr := serveRedis(t)
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		rd := bufio.NewReader(conn)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		for _, line := range []string{
			fmt.Sprintf("%c%d\t%d\t0\t%s\td\n", proxy.OpHello, proxy.VersionMajor, proxy.VersionMinor, user),
			fmt.Sprintf("%c1\t%s\n", proxy.OpBegin, user),
			fmt.Sprintf("%c1\tpriv/old\tv\n", proxy.OpSet),
			fmt.Sprintf("%c1\n", proxy.OpCommit),
		} {
			if _, err := conn.Write([]byte(line)); err != nil {
				t.Fatal(err)
			}
			if line[0] != proxy.OpHello {
				reply, err := rd.ReadString('\n')
				if err != nil || reply[0] == proxy.ReplyFail {
					t.Fatalf("%q: reply %q err %v", line, reply, err)
				}
			}
		}
		if got := mr.TTL(onlyKey(t, mr, "/old")); got != 0 {
			t.Fatalf("TTL %v; a two-field BEGIN must leave the key without one", got)
		}
	})
}
