package warden

import (
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// One client keeps IMAP sessions and a JMAP push connection from one address;
// each protocol counts against its own limit, as the reference counts them
// (#2457). Both backends, since Redis keys the counter itself.
func TestTheUserIPLimitIsCountedPerProtocol(t *testing.T) {
	const u, ip = "alice@example.com", "192.0.2.7"
	type step struct {
		service string
		n       int  // connects of this service, in order
		lastOK  bool // whether the last of them passes; the others must
	}
	tests := []struct {
		name   string
		limits ConnLimits
		steps  []step
	}{
		{"ten IMAP and one JMAP pass at the default 10, an eleventh IMAP does not",
			ConnLimits{Default: 10},
			[]step{{"imap", 10, true}, {"jmap", 1, true}, {"imap", 1, false}}},
		{"JMAP at 2 refuses a third JMAP beside ten live IMAP",
			ConnLimits{Default: 10, Protocols: map[string]int{"jmap": 2}},
			[]step{{"imap", 10, true}, {"jmap", 2, true}, {"jmap", 1, false}}},
		{"0 for IMAP lifts IMAP's limit only",
			ConnLimits{Default: 2, Protocols: map[string]int{"imap": 0}},
			[]step{{"imap", 15, true}, {"jmap", 2, true}, {"jmap", 1, false}}},
		{"a protocol named in upper case counts as the same protocol",
			ConnLimits{Default: 10, Protocols: map[string]int{"jmap": 1}},
			[]step{{"JMAP", 1, true}, {"jmap", 1, false}}},
	}
	backends := map[string]func(t *testing.T, l ConnLimits) StateBackend{
		"memory": func(_ *testing.T, l ConnLimits) StateBackend { return newMemoryBackend(time.Minute, time.Minute, l) },
		"redis": func(t *testing.T, l ConnLimits) StateBackend {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			return NewRedisBackend(rdb, "test:warden:", "test:warden:events:", time.Minute, time.Minute, l)
		},
	}
	for bname, mk := range backends {
		for _, tc := range tests {
			t.Run(bname+"/"+tc.name, func(t *testing.T) {
				b := mk(t, tc.limits)
				defer b.Close()
				id := 0
				for _, st := range tc.steps {
					for i := 1; i <= st.n; i++ {
						id++
						want := i < st.n || st.lastOK
						ok, err := b.SessionConnect(fmt.Sprintf("s%d", id), u, ip, st.service)
						if err != nil || ok != want {
							t.Fatalf("%s connect %d of %d = (%v, %v), want (%v, nil)", st.service, i, st.n, ok, err, want)
						}
					}
				}
			})
		}
	}
}

// A disconnect frees a slot of its own protocol only: closing an IMAP session
// does not make room for a JMAP connection over its limit.
func TestADisconnectFreesItsOwnProtocolsSlot(t *testing.T) {
	const u, ip = "alice@example.com", "192.0.2.7"
	backends := map[string]func(t *testing.T) StateBackend{
		"memory": func(*testing.T) StateBackend {
			return newMemoryBackend(time.Minute, time.Minute, ConnLimits{Default: 1})
		},
		"redis": func(t *testing.T) StateBackend {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			return NewRedisBackend(rdb, "test:warden:", "test:warden:events:", time.Minute, time.Minute, ConnLimits{Default: 1})
		},
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			b := mk(t)
			defer b.Close()
			for _, c := range []struct {
				id, service string
				want        bool
			}{{"i1", "imap", true}, {"j1", "jmap", true}, {"j2", "jmap", false}} {
				if ok, err := b.SessionConnect(c.id, u, ip, c.service); err != nil || ok != c.want {
					t.Fatalf("connect %s = (%v, %v), want %v", c.id, ok, err, c.want)
				}
			}
			b.SessionDisconnect("i1", u, ip)
			if ok, _ := b.SessionConnect("j3", u, ip, "jmap"); ok {
				t.Fatal("an IMAP disconnect freed a JMAP slot")
			}
			if ok, _ := b.SessionConnect("i2", u, ip, "imap"); !ok {
				t.Fatal("the IMAP disconnect did not free its own slot")
			}
			b.SessionDisconnect("j1", u, ip)
			if ok, _ := b.SessionConnect("j4", u, ip, "jmap"); !ok {
				t.Fatal("the JMAP disconnect did not free its own slot")
			}
			d, err := b.Dump()
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int{}
			for _, c := range d.Counters {
				if c.UserIP == u+"@"+ip {
					got[c.Service] = c.Counter
				}
			}
			if got["imap"] != 1 || got["jmap"] != 1 || len(got) != 2 {
				t.Fatalf("DUMP counters by protocol = %v, want imap 1 and jmap 1", got)
			}
		})
	}
}
