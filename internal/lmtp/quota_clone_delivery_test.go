package lmtp

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	_ "github.com/yarilomail/yarilo/pkg/dict/memory"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// countingDict counts the transactions the mirror commits.
type countingDict struct {
	dict.Dict
	commits atomic.Int64
}

func (c *countingDict) Begin(ctx context.Context, set *dict.OpSettings) (dict.Tx, error) {
	tx, err := c.Dict.Begin(ctx, set)
	if err != nil {
		return nil, err
	}
	return &countingTx{Tx: tx, owner: c}, nil
}

type countingTx struct {
	dict.Tx
	owner *countingDict
}

func (t *countingTx) Commit() (dict.CommitResult, error) {
	t.owner.commits.Add(1)
	return t.Tx.Commit()
}

// A delivery is where usage changes most, and it must not wait for the mirror:
// the dict sees nothing while the message is being accepted, and the value
// arrives on the mirror's own timer (#1875).
func TestADeliveryDoesNotWriteTheMirror(t *testing.T) {
	dir := t.TempDir()
	// A limit, because the mirror is only updated where usage is counted.
	resolver := &mailbox.Resolver{
		Root: dir, HomeTemplate: "%d/%n",
		DefaultQuotaRules: []string{"*:storage=1000000"},
	}
	info := resolver.UserInfo("alice@example.com", "")
	info.Driver = "maildir"
	mb := maildir.New()
	box := mb.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	box.Close() //nolint:errcheck

	inner, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	counted := &countingDict{Dict: inner}
	clone := quota.NewClone([]dict.Dict{counted}, 300*time.Millisecond)

	srv := New(Options{
		Hostname:    "lmtp.test",
		Config:      config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
		Mailbox:     mb,
		Index:       fileindex.New(),
		QuotaEngine: true,
		QuotaClone:  clone,
		Resolver:    resolver,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() { _ = srv.Serve(ln) }()

	conn, sc := dialLMTP(t, ln.Addr().String())
	sendLHLO(t, conn, sc)
	resp := deliver(t, conn, sc, "sender@external.com", "alice@example.com", testMsg)
	if len(resp) == 0 || resp[0][0] != '2' {
		t.Fatalf("the delivery was refused: %v", resp)
	}

	if n := counted.commits.Load(); n != 0 {
		t.Errorf("the delivery wrote the mirror %d times on its own path, want 0", n)
	}

	deadline := time.Now().Add(5 * time.Second)
	for counted.commits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the mirror never received the delivery's usage")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
