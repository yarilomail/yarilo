package mdbox

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Two backends on one home stand for two processes; every record must read back
// as itself, not as a peer that took the same offset (#2234).
func TestConcurrentSavesFromTwoProcessesKeepTheirOwnBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"lock service", []Option{WithLocker(&mutexLocker{})}},
		// Without the service, sessions of one process still serialise.
		{"no lock service", nil},
	} {
		t.Run(tc.name, func(t *testing.T) { concurrentSaves(t, tc.opts) })
	}
}

func concurrentSaves(t *testing.T, opts []Option) {
	home := t.TempDir()
	var users []*userMailbox
	for i := range 2 {
		u := New(opts...).OpenUser(&mailbox.UserInfo{
			Username: "alice@example.com", Home: home, SessionID: fmt.Sprintf("s%d", i),
		}).(*userMailbox)
		if err := u.Init(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = u.Close() })
		users = append(users, u)
	}

	const workers, saves = 4, 25
	type saved struct{ name, body string }
	var mu sync.Mutex
	var got []saved
	var wg sync.WaitGroup
	for p, u := range users {
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range saves {
					// Distinct lengths, so a record read at a neighbour's offset
					// cannot match by accident.
					body := fmt.Sprintf("Subject: p%d w%d i%d\r\n\r\n%s\r\n", p, w, i, strings.Repeat("x", p*1000+w*100+i))
					name, _, _, err := u.Save("INBOX", strings.NewReader(body), 0, 0, nil, nil, [16]byte{})
					if err != nil {
						t.Errorf("save p%d w%d i%d: %v", p, w, i, err)
						return
					}
					mu.Lock()
					got = append(got, saved{name, body})
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()

	reader := users[0]
	for _, s := range got {
		rc, err := reader.Fetch("INBOX", s.name, false)
		if err != nil {
			t.Errorf("fetch %s: %v", s.name, err)
			continue
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Errorf("read %s: %v", s.name, err)
			continue
		}
		if string(b) != s.body {
			t.Errorf("map_uid %s reads %q, saved %q", s.name, firstLine(string(b)), firstLine(s.body))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\r'); i >= 0 {
		return s[:i]
	}
	return s
}

// A map entry naming another record's offset is refused, not served as this
// message (#2234).
func TestFetchRefusesARecordWhoseTrailerGUIDDiffers(t *testing.T) {
	mb, _ := newTestUser(t)
	u := mb.(*userMailbox)
	var names []string
	for _, body := range []string{"Subject: a\r\n\r\nfirst\r\n", "Subject: b\r\n\r\nsecond\r\n"} {
		name, _, _, err := u.Save("INBOX", strings.NewReader(body), 0, 0, nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	m, err := u.openMap()
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]mdboxmap.MapEntry, 2)
	for i, name := range names {
		uid, _ := parseFilename(name)
		e, ok, err := m.Lookup(uid)
		if err != nil || !ok {
			t.Fatalf("lookup %s: %v %v", name, ok, err)
		}
		entries[i] = e
	}
	stray, err := m.AppendRecord(entries[0].FileID, entries[1].Offset, entries[1].Size, entries[0].GUID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = u.Fetch("INBOX", strconv.FormatUint(uint64(stray), 10), false)
	if !errors.Is(err, mailbox.ErrCorruptStorage) {
		t.Fatalf("fetch of a record at another message's offset: %v, want ErrCorruptStorage", err)
	}
}
