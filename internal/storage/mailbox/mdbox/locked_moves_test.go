package mdbox

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// lockedUser opens one process's view of alice's store against the lock
// service, as each backend pod does.
func lockedUser(t *testing.T, home string, locker locks.Locker) *userMailbox {
	t.Helper()
	u := New(WithLocker(locker)).OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home, SessionID: fmt.Sprint(time.Now().UnixNano())}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = u.Close() })
	return u
}

func saveIn(t *testing.T, u *userMailbox, folder, body string) (string, [16]byte) {
	t.Helper()
	if err := u.Create(folder); err != nil {
		t.Fatal(err)
	}
	name, _, guid, err := u.Save(folder, strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	return name, guid
}

func refcount(t *testing.T, u *userMailbox, name string) uint16 {
	t.Helper()
	uid, err := parseFilename(name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := u.openMap()
	if err != nil {
		t.Fatal(err)
	}
	e, ok, err := m.LookupReloaded(uid)
	if err != nil || !ok {
		t.Fatalf("map_uid %d: ok=%v err=%v", uid, ok, err)
	}
	return e.RefCount
}

// A move under the lock service keeps the body and the GUID, and the source
// loses its reference.
func TestAMoveUnderTheLockServiceKeepsBodyAndGUID(t *testing.T) {
	dial := lockstest.Server(t)
	u := lockedUser(t, t.TempDir(), dial())
	src, guid := saveIn(t, u, "INBOX", "Subject: m\r\n\r\nmoved body\r\n")
	if err := u.Create("Archive"); err != nil {
		t.Fatal(err)
	}

	dst, got, err := u.Move("INBOX", "Archive", src, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if got != guid {
		t.Errorf("guid %x after move, want %x", got, guid)
	}
	if b := readBody(t, u, dst); !strings.Contains(b, "moved body") {
		t.Errorf("moved body %q", b)
	}
	if rc := refcount(t, u, src); rc != 0 {
		t.Errorf("source refcount %d after move, want 0", rc)
	}
}

// Two processes moving opposite ways between the same two folders finish: the
// pair of folder locks is taken in one order.
func TestOppositeMovesBetweenTwoFoldersDoNotDeadlock(t *testing.T) {
	dial := lockstest.Server(t)
	home := t.TempDir()
	p1, p2 := lockedUser(t, home, dial()), lockedUser(t, home, dial())
	const n = 20
	var inA, inB []string
	for i := range n {
		a, _ := saveIn(t, p1, "A", fmt.Sprintf("Subject: a%d\r\n\r\na\r\n", i))
		b, _ := saveIn(t, p1, "B", fmt.Sprintf("Subject: b%d\r\n\r\nb\r\n", i))
		inA, inB = append(inA, a), append(inB, b)
	}

	done := make(chan error, 2)
	var wg sync.WaitGroup
	for _, run := range []struct {
		u        *userMailbox
		from, to string
		names    []string
	}{{p1, "A", "B", inA}, {p2, "B", "A", inB}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, name := range run.names {
				if _, _, err := run.u.Move(run.from, run.to, name, [16]byte{}); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("opposite moves did not finish in 20 s")
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

// A move inside a held folder takes no second hold of its own, and a move
// within one folder takes its lock once.
func TestAMoveInsideAHeldFolderReusesTheHold(t *testing.T) {
	dial := lockstest.Server(t)
	u := lockedUser(t, t.TempDir(), dial())
	one, _ := saveIn(t, u, "INBOX", "Subject: 1\r\n\r\none\r\n")
	two, _ := saveIn(t, u, "Archive", "Subject: 2\r\n\r\ntwo\r\n")

	err := u.HoldFolder("INBOX", "test", func() error {
		_, _, err := u.Move("INBOX", "Archive", one, [16]byte{})
		return err
	})
	if err != nil {
		t.Fatalf("move inside the INBOX hold: %v", err)
	}
	err = u.HoldFolder("INBOX", "test", func() error {
		return u.HoldFolder("Archive", "test", func() error {
			_, _, err := u.Move("Archive", "INBOX", two, [16]byte{})
			return err
		})
	})
	if err != nil {
		t.Fatalf("move inside both holds: %v", err)
	}
	name, _ := saveIn(t, u, "INBOX", "Subject: 3\r\n\r\nthree\r\n")
	moved, _, err := u.Move("INBOX", "INBOX", name, [16]byte{})
	if err != nil {
		t.Fatalf("move within INBOX: %v", err)
	}
	if b := readBody(t, u, moved); !strings.Contains(b, "three") {
		t.Errorf("body after a move within one folder: %q", b)
	}
}

// A name this driver did not mint is refused by every verb that takes one,
// not read as map_uid 0.
func TestNamesThisDriverDidNotMintAreRefused(t *testing.T) {
	u := lockedUser(t, t.TempDir(), nil)
	saveIn(t, u, "INBOX", "Subject: x\r\n\r\nx\r\n")
	const bad = "1700000000.M1P2.host"
	for verb, call := range map[string]func() error{
		"fetch":  func() error { _, err := u.Fetch("INBOX", bad, false); return err },
		"remove": func() error { return u.Remove("INBOX", bad) },
		"remove many": func() error {
			return u.RemoveManyHeld("INBOX", []string{bad})
		},
		"restore orig":  func() error { return u.RestoreMoved("INBOX", bad, "A", "1", nil) },
		"restore moved": func() error { return u.RestoreMoved("INBOX", "1", "A", bad, nil) },
		"copy":          func() error { _, err := u.Copy("INBOX", bad, "A", 0); return err },
		"move":          func() error { _, _, err := u.Move("INBOX", "A", bad, [16]byte{}); return err },
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "invalid mdbox filename") {
			t.Errorf("%s %q: %v", verb, bad, err)
		}
	}
	if err := u.RemoveManyHeld("INBOX", nil); err != nil {
		t.Errorf("remove of no names: %v", err)
	}
}

// RestoreMoved puts a failed move back: the source is referenced again and
// the copy the move made is not.
func TestRestoreMovedPutsTheSourceBack(t *testing.T) {
	u := lockedUser(t, t.TempDir(), nil)
	src, _ := saveIn(t, u, "INBOX", "Subject: r\r\n\r\nrestore\r\n")
	if err := u.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	dst, _, err := u.Move("INBOX", "Archive", src, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.RestoreMoved("INBOX", src, "Archive", dst, nil); err != nil {
		t.Fatal(err)
	}
	if rc := refcount(t, u, src); rc != 1 {
		t.Errorf("source refcount %d after restore, want 1", rc)
	}
	if rc := refcount(t, u, dst); rc != 0 {
		t.Errorf("moved copy refcount %d after restore, want 0", rc)
	}
	if b := readBody(t, u, src); !strings.Contains(b, "restore") {
		t.Errorf("source body after restore: %q", b)
	}
	if err := u.RemoveHeld("INBOX", src); err != nil {
		t.Fatal(err)
	}
	if rc := refcount(t, u, src); rc != 0 {
		t.Errorf("refcount %d after RemoveHeld, want 0", rc)
	}
}
