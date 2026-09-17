package protocol

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// targetUserdbWithFields knows one target and answers with a mail location that
// is nothing like the master's: a row on equal values would not tell them apart.
type targetUserdbWithFields struct{}

func (targetUserdbWithFields) Lookup(username string) (*UserInfo, error) {
	if username != "alice" {
		return nil, nil
	}
	return &UserInfo{Username: "alice", Home: "/srv/h/alice", MailLocation: "mdbox:/srv/m/alice"}, nil
}

func masterServer(t *testing.T, enabled bool) (net.Conn, *bufio.Scanner) {
	t.Helper()
	opts := []ServerOption{
		WithMasterdb([]Passdb{&credPassdb{"admin", "masterpass"}}),
		WithUserdb(targetUserdbWithFields{}),
	}
	if enabled {
		opts = append(opts, WithMasterUsers(true))
	}
	// admin is a valid user in its own right: dropping the authzid would log the
	// client in as admin, which is exactly what must not happen.
	srv := NewServer([]Passdb{&credPassdb{"alice", "alicepass"}, &credPassdb{"admin", "masterpass"}}, opts...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)
	conn, sc := dialAndHandshake(t, addr)
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck
	return conn, sc
}

func ask(t *testing.T, conn net.Conn, sc *bufio.Scanner, id, resp string) string {
	t.Helper()
	fmt.Fprintf(conn, "AUTH\t%s\tPLAIN\tservice=imap\tresp=%s\n", id, resp)
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	return sc.Text()
}

// A client asking to act as someone else must be refused, not quietly logged in
// as the identity it proved (RFC 4616 §2, #1892).
func TestADistinctAuthzidIsRefusedWithoutMasterUsers(t *testing.T) {
	conn, sc := masterServer(t, false)
	if got := ask(t, conn, sc, "1", "alice\x00admin\x00masterpass"); !strings.HasPrefix(got, "FAIL\t1") {
		t.Errorf("answer %q, want a refusal", got)
	}
}

// The same request with master users on is the one that must succeed, as the
// target -- otherwise the row above would pass on a server that refuses all.
func TestTheSameRequestSucceedsAsTheTargetWithMasterUsers(t *testing.T) {
	conn, sc := masterServer(t, true)
	got := ask(t, conn, sc, "2", "alice\x00admin\x00masterpass")
	if !strings.HasPrefix(got, "OK\t2") || !strings.Contains(got, "user=alice") {
		t.Errorf("answer %q, want OK for user=alice", got)
	}
}

// A master may not enter a name the userdb does not know: the session would
// resolve a mailbox from global defaults for a user that does not exist (#1893).
func TestAnUnknownTargetIsRefused(t *testing.T) {
	conn, sc := masterServer(t, true)
	if got := ask(t, conn, sc, "3", "nosuch\x00admin\x00masterpass"); !strings.HasPrefix(got, "FAIL\t3") {
		t.Errorf("answer %q, want a refusal", got)
	}
}

// The answer must carry the target's storage, not the master's: a master session
// reads the target's mail (#1893).
func TestTheAnswerCarriesTheTargetsUserdbFields(t *testing.T) {
	conn, sc := masterServer(t, true)
	got := ask(t, conn, sc, "4", "alice\x00admin\x00masterpass")
	for _, want := range []string{"user=alice", "/srv/h/alice", "mdbox:/srv/m/alice"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer %q does not carry %q", got, want)
		}
	}
}
