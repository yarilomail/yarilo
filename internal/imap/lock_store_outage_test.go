package imap_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/v2/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/v2/internal/imap"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// storeOutage is a lock store that can be cut off under a running yarilo-locks.
type storeOutage struct {
	*locks.MemoryBackend
	down atomic.Bool
}

var errStoreGone = errors.New("store: connection refused")

func (b *storeOutage) Acquire(ctx context.Context, resource, owner, site, ticket string, ttl time.Duration) (string, locks.Holder, error) {
	if b.down.Load() {
		return "", locks.Holder{}, errStoreGone
	}
	return b.MemoryBackend.Acquire(ctx, resource, owner, site, ticket, ttl)
}

func (b *storeOutage) AcquireShared(ctx context.Context, resource, owner, site, ticket string, ttl time.Duration) (string, locks.Holder, error) {
	if b.down.Load() {
		return "", locks.Holder{}, errStoreGone
	}
	return b.MemoryBackend.AcquireShared(ctx, resource, owner, site, ticket, ttl)
}

// While the lock service cannot reach its store, a command that has to take a
// lock is told to try again, NO [UNAVAILABLE], and not that the server is
// broken (#2503).
func TestALockStoreOutageAnswersUnavailable(t *testing.T) {
	backend := &storeOutage{MemoryBackend: locks.NewMemoryBackend()}
	t.Cleanup(func() { _ = backend.Close() })
	lln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lsrv := locks.NewServer(backend, slog.New(slog.NewTextHandler(io.Discard, nil)), locks.NewMetrics(prometheus.NewRegistry(), "test"))
	lctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = lsrv.Serve(lctx, lln) }()
	t.Cleanup(func() { stop(); lsrv.Close(); <-done })
	locker, err := locks.NewClient(context.Background(), locks.DialTCP(lln.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })

	dir := t.TempDir()
	srv := imapserver.New(imapserver.Options{
		Mailbox:   mdbox.New(mdbox.WithLocker(locker)),
		Index:     file.New(file.WithLocker(locker)),
		Resolver:  &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		AuthRelay: authtest.RelayTo(t, &quotaAuthStub{user: "user@test.com", pass: "testpass", rule: "*:bytes=1000000"}),
		Locker:    locker,
	})
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { _ = ln.Close() })
	conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	appendOne := func() error {
		body := []byte("From: a@b.test\r\nSubject: one\r\n\r\nbody\r\n")
		ac := c.Append("INBOX", int64(len(body)), nil)
		if _, err := ac.Write(body); err != nil {
			return err
		}
		if err := ac.Close(); err != nil {
			return err
		}
		_, err := ac.Wait()
		return err
	}
	if err := appendOne(); err != nil {
		t.Fatalf("append while the store is up: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	deleted := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}, Silent: true}
	if err := c.Store(imap.SeqSetNum(1), deleted, nil).Close(); err != nil {
		t.Fatalf("store while the store is up: %v", err)
	}

	backend.down.Store(true)
	tests := []struct {
		command string
		run     func() error
	}{
		{"APPEND", appendOne},
		{"EXPUNGE", func() error { _, err := c.Expunge().Collect(); return err }},
	}
	for _, tc := range tests {
		err := tc.run()
		var imapErr *imap.Error
		if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeUnavailable {
			t.Errorf("%s with the lock store down = %v, want NO [UNAVAILABLE]", tc.command, err)
		}
	}
}
