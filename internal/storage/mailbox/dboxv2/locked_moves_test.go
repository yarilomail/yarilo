package dboxv2

import (
	"fmt"
	"io"
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
func lockedUser(t *testing.T, home string, locker locks.Locker) mailbox.UserMailbox {
	t.Helper()
	mb := New(WithLocker(locker)).OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	if err := mb.Init(); err != nil {
		t.Fatal(err)
	}
	return mb
}

func bodyOf(t *testing.T, mb mailbox.UserMailbox, folder, name string) string {
	t.Helper()
	rc, err := mb.Fetch(folder, name, false)
	if err != nil {
		t.Fatalf("fetch %s/%s: %v", folder, name, err)
	}
	defer rc.Close() //nolint:errcheck
	b, _ := io.ReadAll(rc)
	return string(b)
}

// Two processes moving opposite ways between the same two folders finish: the
// pair of folder locks is taken in one order.
func TestOppositeMovesBetweenTwoFoldersDoNotDeadlock(t *testing.T) {
	dial := lockstest.Server(t)
	home := t.TempDir()
	p1, p2 := lockedUser(t, home, dial()), lockedUser(t, home, dial())
	for _, f := range []string{"A", "B"} {
		if err := p1.Create(f); err != nil {
			t.Fatal(err)
		}
	}
	const n = 20
	var inA, inB []string
	for i := range n {
		a, _ := saveNamed(t, p1, "A", fmt.Sprintf("Subject: a%d\r\n\r\na\r\n", i), uint32(i+1), [16]byte{})
		b, _ := saveNamed(t, p1, "B", fmt.Sprintf("Subject: b%d\r\n\r\nb\r\n", i), uint32(i+1), [16]byte{})
		inA, inB = append(inA, a), append(inB, b)
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, run := range []struct {
		mb       mailbox.UserMailbox
		from, to string
		names    []string
	}{{p1, "A", "B", inA}, {p2, "B", "A", inB}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, name := range run.names {
				if _, _, err := run.mb.Move(run.from, run.to, name, [16]byte{}); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("opposite moves did not finish in 20 s")
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

// A move inside the source's hold, as IMAP MOVE makes it, takes no second hold
// of its own, and a move within one folder takes its lock once.
func TestAMoveInsideAHeldFolderReusesTheHold(t *testing.T) {
	dial := lockstest.Server(t)
	mb := lockedUser(t, t.TempDir(), dial())
	if err := mb.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	one, _ := saveNamed(t, mb, "INBOX", "Subject: 1\r\n\r\none\r\n", 1, [16]byte{})
	two, _ := saveNamed(t, mb, "Archive", "Subject: 2\r\n\r\ntwo\r\n", 1, [16]byte{})
	holder := mb.(interface {
		HoldFolder(folder, site string, fn func() error) error
	})
	var moved string
	err := holder.HoldFolder("INBOX", "test", func() error {
		var err error
		moved, _, err = mb.Move("INBOX", "Archive", one, [16]byte{})
		return err
	})
	if err != nil {
		t.Fatalf("move inside the source's hold: %v", err)
	}
	if b := bodyOf(t, mb, "Archive", moved); !strings.Contains(b, "one") {
		t.Errorf("moved body %q", b)
	}
	same, _, err := mb.Move("Archive", "Archive", two, [16]byte{})
	if err != nil {
		t.Fatalf("move within Archive: %v", err)
	}
	if b := bodyOf(t, mb, "Archive", same); !strings.Contains(b, "two") {
		t.Errorf("body after a move within one folder: %q", b)
	}
}
