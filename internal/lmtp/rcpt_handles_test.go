package lmtp

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"

	fileindex "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// countingIndex counts the recipient indexes a connection opens and closes.
type countingIndex struct {
	inner         mailbox.IndexBackend
	opens, closes *atomic.Int64
	closed        chan struct{}
}

func (b countingIndex) OpenUser(ui *mailbox.UserInfo) mailbox.UserIndex {
	b.opens.Add(1)
	return closeCounted{UserIndex: b.inner.OpenUser(ui), closes: b.closes, closed: b.closed}
}

type closeCounted struct {
	mailbox.UserIndex
	closes *atomic.Int64
	closed chan struct{}
}

func (c closeCounted) Close() error {
	c.closes.Add(1)
	if c.closed != nil {
		select {
		case c.closed <- struct{}{}:
		default:
		}
	}
	return c.UserIndex.Close()
}

func heldServer(t *testing.T, opens, closes *atomic.Int64) string {
	t.Helper()
	return heldServerSignalling(t, opens, closes, nil)
}

func heldServerSignalling(t *testing.T, opens, closes *atomic.Int64, closed chan struct{}) string {
	t.Helper()
	dir := t.TempDir()
	srv := New(testOpts(Options{
		Hostname: "lmtp.test",
		Config:   config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
		Mailbox:  maildir.New(),
		Index:    countingIndex{inner: fileindex.New(), opens: opens, closes: closes, closed: closed},
		Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		UserdbLookup: func(_ context.Context, user string) (*mailbox.UserInfo, error) {
			home := filepath.Join(dir, user)
			return &mailbox.UserInfo{Username: user, Home: home, MailPath: filepath.Join(home, "Maildir")}, nil
		},
	}))
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String()
}

// quit ends the session and reads it to its end: the server closes the
// connection after Logout, so the counters stand once the reads stop.
func quit(conn net.Conn, sc interface{ Scan() bool }) {
	fmt.Fprintf(conn, "QUIT\r\n")
	for sc.Scan() {
	}
}

// A connection opens a recipient's index once for all its messages, so the
// next one does not parse the folder's index again; QUIT closes it (#2618).
func TestAConnectionKeepsItsRecipientsIndexOpen(t *testing.T) {
	var opens, closes atomic.Int64
	conn, sc := dialLMTP(t, heldServer(t, &opens, &closes))
	sendLHLO(t, conn, sc)
	for i := 0; i < 3; i++ {
		if resp := deliver(t, conn, sc, "s@external.com", "alice@example.com", testMsg); len(resp) == 0 || resp[0][0] != '2' {
			t.Fatalf("delivery %d: %v", i+1, resp)
		}
	}
	if got := opens.Load(); got != 1 {
		t.Errorf("alice's index opened %d times for three messages, want once", got)
	}
	if got := closes.Load(); got != 0 {
		t.Errorf("alice's index closed %d times while the connection lasts, want 0", got)
	}
	quit(conn, sc)
	if got := closes.Load(); got != 1 {
		t.Errorf("closed %d after QUIT, want alice's one", got)
	}
}

// Past rcptHandlesMax recipients the one used longest ago is closed: a relay's
// connection does not hold every recipient it ever served.
func TestAConnectionHoldsAtMostItsBoundOfRecipients(t *testing.T) {
	var opens, closes atomic.Int64
	conn, sc := dialLMTP(t, heldServer(t, &opens, &closes))
	sendLHLO(t, conn, sc)
	for i := 0; i <= rcptHandlesMax; i++ {
		if resp := deliver(t, conn, sc, "s@external.com", fmt.Sprintf("u%d@example.com", i), testMsg); len(resp) == 0 || resp[0][0] != '2' {
			t.Fatalf("delivery to u%d: %v", i, resp)
		}
	}
	if got := opens.Load(); got != rcptHandlesMax+1 {
		t.Errorf("opened %d indexes for %d recipients", got, rcptHandlesMax+1)
	}
	if got := closes.Load(); got != 1 {
		t.Errorf("closed %d while the connection lasts, want the one past the bound", got)
	}
	quit(conn, sc)
	if got := closes.Load(); got != rcptHandlesMax+1 {
		t.Errorf("closed %d after QUIT, want all %d", got, rcptHandlesMax+1)
	}
}

// A connection that drops without QUIT closes its recipients' indexes too: the
// server's close runs Logout.
func TestADroppedConnectionClosesItsRecipientsIndexes(t *testing.T) {
	var opens, closes atomic.Int64
	closed := make(chan struct{}, 1)
	conn, sc := dialLMTP(t, heldServerSignalling(t, &opens, &closes, closed))
	sendLHLO(t, conn, sc)
	if resp := deliver(t, conn, sc, "s@external.com", "alice@example.com", testMsg); len(resp) == 0 || resp[0][0] != '2' {
		t.Fatalf("delivery: %v", resp)
	}
	conn.Close() //nolint:errcheck
	select {
	case <-closed:
	case <-t.Context().Done():
	}
	if got := closes.Load(); got != opens.Load() || got != 1 {
		t.Errorf("after the drop: %d opened, %d closed, want 1 and 1", opens.Load(), got)
	}
}

// A held index writes on what another process changed meanwhile: a copy filed
// between two deliveries takes its uid, and the second delivery the next one.
func TestAHeldIndexSeesWhatAnotherProcessWrote(t *testing.T) {
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	srv := New(testOpts(Options{
		Hostname: "lmtp.test",
		Config:   config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
		Mailbox:  maildir.New(),
		Index:    fileindex.New(),
		Resolver: resolver,
	}))
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() { _ = srv.Serve(ln) }()
	conn, sc := dialLMTP(t, ln.Addr().String())
	sendLHLO(t, conn, sc)
	if resp := deliver(t, conn, sc, "s@external.com", "alice@example.com", testMsg); len(resp) == 0 || resp[0][0] != '2' {
		t.Fatalf("first delivery: %v", resp)
	}

	// Another process files a copy, as a delivery through another pod does.
	info, _ := resolver.UserInfo("alice@example.com", "")
	other := fileindex.New().OpenUser(info)
	f, err := other.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := other.AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
		t.Fatal(err)
	}
	other.Close() //nolint:errcheck

	if resp := deliver(t, conn, sc, "s@external.com", "alice@example.com", testMsg); len(resp) == 0 || resp[0][0] != '2' {
		t.Fatalf("second delivery: %v", resp)
	}
	quit(conn, sc)

	check := fileindex.New().OpenUser(info)
	defer check.Close() //nolint:errcheck
	f, err = check.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := check.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	var uids []uint32
	for _, m := range msgs {
		uids = append(uids, m.UID)
	}
	if fmt.Sprint(uids) != fmt.Sprint([]uint32{1, uid, uid + 1}) || uid != 2 {
		t.Errorf("INBOX holds uids %v (the other process took %d), want [1 2 3]", uids, uid)
	}
}

// The quota check before a second delivery counts the first: a held index is
// not a view from before it.
func TestTheQuotaCheckSeesTheMessageBefore(t *testing.T) {
	dir := t.TempDir()
	srv := New(testOpts(Options{
		Hostname:    "lmtp.test",
		Config:      config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
		Mailbox:     maildir.New(),
		Index:       fileindex.New(),
		QuotaEngine: true,
		Resolver:    &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n", DefaultQuotaRules: []string{"*:messages=1"}},
	}))
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() { _ = srv.Serve(ln) }()
	conn, sc := dialLMTP(t, ln.Addr().String())
	sendLHLO(t, conn, sc)
	if resp := deliver(t, conn, sc, "s@external.com", "alice@example.com", testMsg); len(resp) == 0 || resp[0][0] != '2' {
		t.Fatalf("first delivery: %v", resp)
	}
	if resp := deliver(t, conn, sc, "s@external.com", "alice@example.com", testMsg); len(resp) == 0 || resp[0][0] == '2' {
		t.Errorf("the second delivery into a mailbox of one message was accepted: %v", resp)
	}
}
