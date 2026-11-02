package ftsproto

import (
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/fts"
)

// echoService answers every call with values derived from its arguments and
// keeps a line per call, so a test sees both directions of the wire.
type echoService struct {
	mu    sync.Mutex
	calls []string
}

func (e *echoService) note(format string, a ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, fmt.Sprintf(format, a...))
}

func ref(m fts.MailboxRef) string { return fmt.Sprintf("%s/%s/%d", m.Name, m.GUID, m.UIDValidity) }

func (e *echoService) Index(u string, m fts.MailboxRef, max uint32, recent int) error {
	e.note("index %s %s %d %d", u, ref(m), max, recent)
	return nil
}
func (e *echoService) Prepend(u string, m fts.MailboxRef, max uint32) error {
	e.note("prepend %s %s %d", u, ref(m), max)
	return nil
}
func (e *echoService) Expunge(u string, m fts.MailboxRef, uid uint32, guid [16]byte) error {
	e.note("expunge %s %s %d %x", u, ref(m), uid, guid[:2])
	return nil
}
func (e *echoService) Lookup(u string, m fts.MailboxRef, q fts.Query) (fts.Result, error) {
	e.note("lookup %s %s %s", u, ref(m), q.Terms[0].Phrase)
	return fts.Result{Definite: []uint32{3}, Maybe: []uint32{4}}, nil
}
func (e *echoService) Status(u string, m fts.MailboxRef) (uint32, uint32, error) {
	e.note("status %s %s", u, ref(m))
	return 11, 22, nil
}
func (e *echoService) Rescan(u string, m fts.MailboxRef) error {
	e.note("rescan %s %s", u, ref(m))
	return nil
}
func (e *echoService) RescanUser(u string) ([]string, error) {
	e.note("rescan-user %s", u)
	return []string{"INBOX", "Sent"}, nil
}
func (e *echoService) Counts(u string) (uint64, uint64, uint64, uint64, error) {
	e.note("counts %s", u)
	return 1, 2, 3, 4, nil
}
func (e *echoService) Optimize(u string) error {
	e.note("optimize %s", u)
	return nil
}
func (e *echoService) DropFolder(u string, m fts.MailboxRef) error {
	e.note("drop %s %s", u, ref(m))
	return nil
}
func (e *echoService) LookupIn(u string, ms []fts.MailboxRef, q fts.Query) (fts.SetResult, error) {
	e.note("lookup-in %s %d %s", u, len(ms), q.Terms[0].Phrase)
	return fts.SetResult{Definite: []fts.FolderHit{{Folder: ms[0].GUID, UID: 9}}}, nil
}

// Every call crosses the wire with its arguments whole and its answer whole,
// through each kind of client a process can hold.
func TestEveryCallCrossesTheWire(t *testing.T) {
	svc := &echoService{}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go Serve(ln, svc) //nolint:errcheck
	addr := ln.Addr().String()

	remote, err := Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pool := NewPool(addr, nil, 2, time.Second)
	lazy := NewLazy(addr, nil, time.Second)
	t.Cleanup(func() { _ = remote.Close(); _ = pool.Close(); _ = lazy.Close() })
	if pool.Size() != 2 {
		t.Errorf("pool size %d, want 2", pool.Size())
	}

	m := fts.MailboxRef{Name: "Sent Items", GUID: "g1", UIDValidity: 7}
	q := fts.Query{Terms: []fts.Term{{Phrase: "two words"}}}
	want := []string{
		"index u@x Sent Items/g1/7 50 10", "prepend u@x Sent Items/g1/7 51",
		"expunge u@x Sent Items/g1/7 5 abcd", "lookup u@x Sent Items/g1/7 two words",
		"status u@x Sent Items/g1/7", "rescan u@x Sent Items/g1/7", "rescan-user u@x",
		"counts u@x", "optimize u@x", "drop u@x Sent Items/g1/7", "lookup-in u@x 2 two words",
	}
	for name, c := range map[string]Client{"remote": remote, "pool": pool, "lazy": lazy, "embedded": Embedded{svc}} {
		t.Run(name, func(t *testing.T) {
			svc.mu.Lock()
			svc.calls = nil
			svc.mu.Unlock()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(c.Index("u@x", m, 50, 10))
			must(c.Prepend("u@x", m, 51))
			must(c.Expunge("u@x", m, 5, [16]byte{0xab, 0xcd}))
			res, err := c.Lookup("u@x", m, q)
			must(err)
			if !reflect.DeepEqual(res.Definite, []uint32{3}) || !reflect.DeepEqual(res.Maybe, []uint32{4}) {
				t.Errorf("lookup = %+v", res)
			}
			last, sum, err := c.Status("u@x", m)
			if err != nil || last != 11 || sum != 22 {
				t.Errorf("status = %d %d %v", last, sum, err)
			}
			must(c.Rescan("u@x", m))
			done, err := c.RescanUser("u@x")
			if err != nil || strings.Join(done, ",") != "INBOX,Sent" {
				t.Errorf("rescan-user = %v %v", done, err)
			}
			d, cp, ms, un, err := c.Counts("u@x")
			if err != nil || d != 1 || cp != 2 || ms != 3 || un != 4 {
				t.Errorf("counts = %d %d %d %d %v", d, cp, ms, un, err)
			}
			must(c.Optimize("u@x"))
			must(c.DropFolder("u@x", m))
			set, err := c.LookupIn("u@x", []fts.MailboxRef{m, {Name: "INBOX", GUID: "g2"}}, q)
			if err != nil || len(set.Definite) != 1 || set.Definite[0] != (fts.FolderHit{Folder: "g1", UID: 9}) {
				t.Errorf("lookup-in = %+v %v", set, err)
			}
			svc.mu.Lock()
			got := append([]string(nil), svc.calls...)
			svc.mu.Unlock()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the service was asked:\n%q\nwant:\n%q", got, want)
			}
		})
	}
	if err := (Embedded{svc}).Close(); err != nil {
		t.Errorf("embedded close: %v", err)
	}
}
