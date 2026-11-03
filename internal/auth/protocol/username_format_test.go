package protocol

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// namesPassdb accepts every login and keeps the names the chain was asked about.
type namesPassdb struct {
	mu    sync.Mutex
	names []string
}

func (p *namesPassdb) Authenticate(req *Request) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.names = append(p.names, req.Username)
	return ResultOK, nil
}

func (p *namesPassdb) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.names...)
}

// field returns the value of key in an OK line, and whether it was there.
func field(reply []string, key string) (string, bool) {
	for _, f := range reply {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// auth_username_format is applied before any passdb is asked: the passdb, the
// canonical user in the answer and the cache all see the formatted name, and
// the name the client gave stays in original_user (#2511).
func TestUsernameFormatBeforeThePassdb(t *testing.T) {
	tests := []struct {
		format       string
		wantAsked    string
		wantOriginal string
	}{
		{format: "%Lu", wantAsked: "bob@keptemail.com", wantOriginal: "Bob@KeptEmail.com"},
		{format: "%u", wantAsked: "Bob@KeptEmail.com"},
		{format: "%n@%Ld", wantAsked: "Bob@keptemail.com", wantOriginal: "Bob@KeptEmail.com"},
	}
	for _, tc := range tests {
		t.Run(tc.format, func(t *testing.T) {
			db := &namesPassdb{}
			s := serveVars(t, NewServer([]Passdb{db}, WithUsernameFormat(tc.format)))
			resp := base64.StdEncoding.EncodeToString([]byte("\x00Bob@KeptEmail.com\x00pw"))
			reply := s.send("AUTH\t%d\tPLAIN\tservice=imap\tresp=" + resp)
			if reply[0] != "OK" {
				t.Fatalf("login = %v", reply)
			}
			if got := db.asked(); len(got) != 1 || got[0] != tc.wantAsked {
				t.Errorf("the passdb was asked about %v, want %q", got, tc.wantAsked)
			}
			if user, _ := field(reply, "user"); user != tc.wantAsked {
				t.Errorf("canonical user = %q, want %q", user, tc.wantAsked)
			}
			original, set := field(reply, "original_user")
			if original != tc.wantOriginal || set != (tc.wantOriginal != "") {
				t.Errorf("original_user = %q (set %v), want %q", original, set, tc.wantOriginal)
			}
		})
	}
}

// Two logins that differ only in case are one cache entry: the second is
// answered from the cache, and the passdb is asked once.
func TestUsernameFormatIsTheCacheKey(t *testing.T) {
	db := &namesPassdb{}
	s := serveVars(t, NewServer([]Passdb{db}, WithUsernameFormat("%Lu"),
		WithCache(NewCache(1<<20, time.Minute, time.Minute))))
	for _, name := range []string{"Bob@KeptEmail.com", "bob@keptemail.com"} {
		resp := base64.StdEncoding.EncodeToString([]byte("\x00" + name + "\x00pw"))
		if reply := s.send("AUTH\t%d\tPLAIN\tservice=imap\tresp=" + resp); reply[0] != "OK" {
			t.Fatalf("login %s = %v", name, reply)
		}
	}
	if got := db.asked(); len(got) != 1 {
		t.Errorf("the passdb was asked %d times (%v), want once: two cache entries for one user", len(got), got)
	}
}

// namesUserdb knows bob@keptemail.com and keeps the names it was asked about.
type namesUserdb struct {
	mu    sync.Mutex
	names []string
}

func (u *namesUserdb) Lookup(name string) (*UserInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.names = append(u.names, name)
	if name != "bob@keptemail.com" {
		return nil, nil
	}
	return &UserInfo{Username: name, Home: "/m/bob"}, nil
}

// A delivery to Bob@KeptEmail.com reaches bob@keptemail.com's mailbox: the
// master listener's USER, which LMTP asks, formats the name first.
func TestUsernameFormatOnDelivery(t *testing.T) {
	udb := &namesUserdb{}
	srv := NewMasterServer(udb, WithMasterUsernameFormat("%Lu"))
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln) //nolint:errcheck
	c, r := dialMaster(t, ln.Addr().String())
	drainHandshake(t, r)
	fmt.Fprintf(c, "USER\t1\tBob@KeptEmail.com\tservice=lmtp\n")
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "USER\t1\tbob@keptemail.com\t") || !strings.Contains(line, "home=/m/bob") {
		t.Errorf("USER Bob@KeptEmail.com = %q, want bob@keptemail.com's entry", line)
	}
	if len(udb.names) != 1 || udb.names[0] != "bob@keptemail.com" {
		t.Errorf("the userdb was asked about %v, want bob@keptemail.com", udb.names)
	}
}

// A SCRAM login is formatted before its credentials are looked up: Alice
// finds alice's verifier, and the answer names alice with Alice as
// original_user.
func TestUsernameFormatOnSCRAM(t *testing.T) {
	db := &scramPassdb{username: "alice", creds: scramCreds(t)}
	s := serveVars(t, NewServer([]Passdb{db}, WithUsernameFormat("%Lu")))
	d := newScramClientDriver(MechScramSha256, "Alice", "pw", nil)
	f := s.send("AUTH\t%d\t%s\tservice=imap\tsession=s1\tresp=%s", MechScramSha256,
		base64.StdEncoding.EncodeToString(d.clientFirst()))
	if f[0] != "CONT" {
		t.Fatalf("first message = %v, want CONT: alice's verifier was not found for Alice", f)
	}
	first, _ := base64.StdEncoding.DecodeString(f[2])
	fmt.Fprintf(s.conn, "CONT\t%d\t%s\n", s.n, base64.StdEncoding.EncodeToString(d.clientFinal(t, first)))
	reply := strings.Split(s.line(), "\t")
	user, _ := field(reply, "user")
	original, _ := field(reply, "original_user")
	if reply[0] != "OK" || user != "alice" || original != "Alice" {
		t.Errorf("SCRAM as Alice = %v, want OK for alice with original_user Alice", reply)
	}
}
