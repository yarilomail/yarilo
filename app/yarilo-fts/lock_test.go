package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// ftsLockService runs the real service: the site is a wire field, and a fake
// locker would assert this test's own idea of it.
func ftsLockService(t *testing.T) func() locks.Locker {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl-fts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	backend := locks.NewMemoryBackend()
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	ln, err := locks.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, derr := net.DialTimeout("unix", sock, 50*time.Millisecond); derr == nil {
			_ = c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		srv.Close()
		cancel()
		_ = backend.Close()
		<-done
	})
	return func() locks.Locker {
		c, cerr := locks.NewClient(context.Background(), locks.DialUnix(sock))
		if cerr != nil {
			t.Fatal(cerr)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}

// An indexing pass announces what it is doing: it took the key with a bare
// context, and no test reached this path to notice (#1679).
func TestAnIndexingPassNamesItsSite(t *testing.T) {
	dial := ftsLockService(t)
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		err := lockMailbox(dial())("u1@example.com", "INBOX", func() error {
			close(held)
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("indexing pass: %v", err)
		}
	}()
	<-held
	defer close(release)

	// Read off a refusal, which is where an operator reads it too.
	other := dial()
	ctx := locks.WithSite(context.Background(), "read")
	lk, err := other.Lock(ctx, locks.FTSKey("u1@example.com", "INBOX"),
		locks.Owner("u1@example.com", "waiter"), time.Minute)
	if !errors.Is(err, locks.ErrBusy) {
		t.Fatalf("second acquisition returned %v, want ErrBusy", err)
	}
	if lk.Site != "fts-index" {
		t.Errorf("an indexing pass holds the key as %q, want \"fts-index\"", lk.Site)
	}
}

// A pass whose user is already held queues for it instead of failing: one
// index per user makes that meeting routine (#1986). Real hold, real service.
func TestAPassWaitsForAHeldIndex(t *testing.T) {
	dial := ftsLockService(t)
	const user = "u1@example.com"
	holder := dial()
	lk, err := holder.Lock(locks.WithSite(context.Background(), "other"),
		locks.FTSKey(user, ""), locks.Owner(user, "holder"), time.Minute)
	if err != nil {
		t.Fatalf("the foreign hold was not taken: %v", err)
	}

	released := make(chan time.Time, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		released <- time.Now()
		_ = holder.Unlock(context.Background(), lk.ID)
	}()

	ran := false
	start := time.Now()
	if err := lockMailbox(dial())(user, "", func() error { ran = true; return nil }); err != nil {
		t.Fatalf("the pass refused to wait: %v", err)
	}
	if !ran {
		t.Error("the pass reported success without running")
	}
	if got := <-released; start.After(got) || time.Since(got) < 0 {
		t.Errorf("the pass ran at %v, before the hold was released at %v", start, got)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("the pass took %v, so it did not wait out the 300ms hold", elapsed)
	}
}

// Past the limit the wait ends as ErrBusy, which is the one error the
// background retry defers on (ftsservice/retry.go); anything else drops it.
func TestAPassPastTheWaitLimitReportsBusy(t *testing.T) {
	old := lockWaitLimit
	lockWaitLimit = 200 * time.Millisecond
	t.Cleanup(func() { lockWaitLimit = old })

	dial := ftsLockService(t)
	const user = "u2@example.com"
	holder := dial()
	if _, err := holder.Lock(locks.WithSite(context.Background(), "other"),
		locks.FTSKey(user, ""), locks.Owner(user, "holder"), time.Minute); err != nil {
		t.Fatalf("the foreign hold was not taken: %v", err)
	}

	err := lockMailbox(dial())(user, "", func() error {
		t.Error("the pass ran while the index was held by someone else")
		return nil
	})
	if !errors.Is(err, locks.ErrBusy) {
		t.Fatalf("a pass past the wait limit returned %v, want ErrBusy", err)
	}
}

// The hold outlives its TTL because it is renewed: one hold now covers a
// whole-user walk, and a lapse would admit a writer mid-walk (#1986).
func TestAHoldIsRenewedWhileThePassRuns(t *testing.T) {
	defer swapLockTiming(t, 600*time.Millisecond, 150*time.Millisecond)()

	dial := ftsLockService(t)
	const user = "u3@example.com"
	inside := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- lockMailbox(dial())(user, "", func() error {
			close(inside)
			<-release
			return nil
		})
	}()

	<-inside
	// Past the TTL, and still ours: an unrenewed hold has expired by now.
	time.Sleep(900 * time.Millisecond)
	if _, err := dial().Lock(locks.WithSite(context.Background(), "other"),
		locks.FTSKey(user, ""), locks.Owner(user, "rival"), time.Minute); !errors.Is(err, locks.ErrBusy) {
		t.Errorf("another writer took the index mid-pass: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Errorf("the pass failed: %v", err)
	}
}

// swapLockTiming shortens the hold's TTL for one row and restores it.
func swapLockTiming(t *testing.T, ttl, renew time.Duration) func() {
	t.Helper()
	oldTTL, oldRenew := lockTTL, lockRenewEvery
	lockTTL, lockRenewEvery = ttl, renew
	return func() { lockTTL, lockRenewEvery = oldTTL, oldRenew }
}
