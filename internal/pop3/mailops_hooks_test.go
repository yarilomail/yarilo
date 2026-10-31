package pop3

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/auth/authtest"
	"github.com/yarilomail/yarilo/v2/internal/quotawarn"
	"github.com/yarilomail/yarilo/v2/pkg/dict"
	_ "github.com/yarilomail/yarilo/v2/pkg/dict/memory"
	"github.com/yarilomail/yarilo/v2/pkg/fts"
	"github.com/yarilomail/yarilo/v2/pkg/quota"
)

// expungeRecorder is an FTS service that hands on what it was told to retract;
// the layer tells it from a goroutine of its own.
type expungeRecorder struct{ uids chan uint32 }

func (r *expungeRecorder) Expunge(_ string, _ fts.MailboxRef, uid uint32, _ [16]byte) error {
	r.uids <- uid
	return nil
}

func (r *expungeRecorder) Index(string, fts.MailboxRef, uint32, int) error { return nil }
func (r *expungeRecorder) Prepend(string, fts.MailboxRef, uint32) error    { return nil }
func (r *expungeRecorder) Lookup(string, fts.MailboxRef, fts.Query) (fts.Result, error) {
	return fts.Result{}, nil
}
func (r *expungeRecorder) Status(string, fts.MailboxRef) (uint32, uint32, error) { return 0, 0, nil }
func (r *expungeRecorder) Rescan(string, fts.MailboxRef) error                   { return nil }
func (r *expungeRecorder) RescanUser(string) ([]string, error)                   { return nil, nil }
func (r *expungeRecorder) Counts(string) (uint64, uint64, uint64, uint64, error) {
	return 0, 0, 0, 0, nil
}
func (r *expungeRecorder) DropFolder(string, fts.MailboxRef) error { return nil }
func (r *expungeRecorder) LookupIn(string, []fts.MailboxRef, fts.Query) (fts.SetResult, error) {
	return fts.SetResult{}, nil
}
func (r *expungeRecorder) Optimize(string) error { return nil }
func (r *expungeRecorder) Close() error          { return nil }

// deleteFirst runs DELE 1 and QUIT, the deletion POP3 commits at QUIT.
func deleteFirst(t *testing.T, o Options) {
	t.Helper()
	c, r := newPOP3Session(t, o)
	login(t, c, r, "u@x", "p")
	send(t, c, "DELE 1")
	if resp := readline(t, r); !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("DELE: %q", resp)
	}
	send(t, c, "QUIT")
	if resp := readline(t, r); !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("QUIT: %q", resp)
	}
}

// A message deleted over POP3 leaves the full-text index, as IMAP's does (#2213).
func TestAPOP3DeletionRetractsTheFTSDocument(t *testing.T) {
	rec := &expungeRecorder{uids: make(chan uint32, 4)}
	o, _ := maildirStandN(t, func(o *Options) { o.FTSClient = rec }, 2)
	deleteFirst(t, o)
	select {
	case uid := <-rec.uids:
		if uid != 1 {
			t.Errorf("the FTS service was told to retract uid %d, want 1", uid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the FTS service was never told to retract the deleted message")
	}
}

// A deletion over POP3 moves the quota warnings: dropping under 90% of the
// limit is the "under" crossing (#2213).
func TestAPOP3DeletionMovesTheQuotaWarnings(t *testing.T) {
	o, cur := maildirStandN(t, func(o *Options) {
		o.QuotaEngine = true
		o.QuotaPolicy = quota.Policy{StoragePercentage: 100, MessagePercentage: 100,
			Warnings: []quota.Warning{{Name: "under90", Resource: "storage", Threshold: "under", Percentage: 90}}}
		o.QuotaWarner = quotawarn.New("", 5)
	}, 2)
	// Two messages of 35 bytes against 72: 97% before, 48% after one goes.
	o.AuthRelay = authtest.RelayTo(t, &mockAuth{users: map[string]string{"u@x": "p"},
		home: filepath.Dir(filepath.Dir(cur)), quotaRules: []string{"*:bytes=72"}})

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	deleteFirst(t, o)
	if logs := buf.String(); !strings.Contains(logs, "quota warning crossed") || !strings.Contains(logs, "threshold=under") {
		t.Fatalf("no under crossing after the POP3 deletion:\n%s", logs)
	}
}

// signalDict hands on every transaction the quota mirror commits.
type signalDict struct {
	dict.Dict
	commits chan struct{}
}

func (d *signalDict) Begin(ctx context.Context, set *dict.OpSettings) (dict.Tx, error) {
	tx, err := d.Dict.Begin(ctx, set)
	if err != nil {
		return nil, err
	}
	return &signalTx{Tx: tx, owner: d}, nil
}

type signalTx struct {
	dict.Tx
	owner *signalDict
}

func (t *signalTx) Commit() (dict.CommitResult, error) {
	res, err := t.Tx.Commit()
	t.owner.commits <- struct{}{}
	return res, err
}

// The session holds the user in the quota mirror, so the value its QUIT
// changed is written when it closes, not an hour later on the timer (#2213).
func TestAPOP3SessionFlushesTheQuotaMirrorOnClose(t *testing.T) {
	inner, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	mirror := &signalDict{Dict: inner, commits: make(chan struct{}, 4)}
	o, cur := maildirStandN(t, func(o *Options) {
		o.QuotaEngine = true
		o.QuotaClone = quota.NewClone([]dict.Dict{mirror}, time.Hour)
	}, 2)
	o.AuthRelay = authtest.RelayTo(t, &mockAuth{users: map[string]string{"u@x": "p"},
		home: filepath.Dir(filepath.Dir(cur)), quotaRules: []string{"*:bytes=1000000"}})
	deleteFirst(t, o)
	select {
	case <-mirror.commits:
	case <-time.After(5 * time.Second):
		t.Fatal("the mirror was not written when the session closed")
	}
}
