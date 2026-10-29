package protocol

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"

	"github.com/yarilomail/yarilo/v2/pkg/sqlvars"
)

// ruledUserdb knows alice except for the refused protocols, as a user_query
// with a CASE on %s does; Lookup is what a lookup without the request sees.
type ruledUserdb struct{ refuse map[string]bool }

func (ruledUserdb) Lookup(username string) (*UserInfo, error) {
	if username != "alice" {
		return nil, nil
	}
	return &UserInfo{Username: "alice"}, nil
}

func (u ruledUserdb) LookupFor(username string, v sqlvars.Vars) (*UserInfo, error) {
	if u.refuse[v.Service] {
		return nil, nil
	}
	return u.Lookup(username)
}

func (ruledUserdb) UsesRequest() bool { return true }

// ruledSCRAM holds alice's verifier; asked with the request it withholds it for
// the refused protocols, asked without it hands it over, as an empty %s would.
type ruledSCRAM struct {
	creds  *sasl.ScramCredentials
	refuse map[string]bool
}

func (ruledSCRAM) Authenticate(*Request) (Result, error) { return ResultNext, nil }

func (p ruledSCRAM) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	if username != "alice" {
		return nil, nil
	}
	return p.creds, nil
}

func (p ruledSCRAM) LookupSCRAMSha256For(username string, v sqlvars.Vars) (*sasl.ScramCredentials, error) {
	if p.refuse[v.Service] {
		return nil, nil
	}
	return p.LookupSCRAMSha256(username)
}

// addressPassdb accepts alice/pw except from one client and at one local
// address, as a password_query on %r and %l does.
type addressPassdb struct{}

func (addressPassdb) Authenticate(req *Request) (Result, error) {
	if req.Username != "alice" || req.Password != "pw" {
		return ResultNext, nil
	}
	if req.RemoteIP == "10.9.9.9" || req.LocalIP == "10.8.8.8" {
		return ResultNext, nil
	}
	req.Fields.Set("user", "alice")
	return ResultOK, nil
}

func (addressPassdb) UsesAddresses() bool { return true }

// varsSession is a raw connection to a served Server.
type varsSession struct {
	t    *testing.T
	conn net.Conn
	rd   *bufio.Reader
	n    int
}

func serveVars(t *testing.T, srv *Server) *varsSession {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln) //nolint:errcheck
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })                 //nolint:errcheck
	conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	fmt.Fprintf(conn, "VERSION\t1\t0\n")
	s := &varsSession{t: t, conn: conn, rd: bufio.NewReader(conn)}
	for s.line() != "DONE" {
	}
	return s
}

func (s *varsSession) line() string {
	s.t.Helper()
	l, err := s.rd.ReadString('\n')
	if err != nil {
		s.t.Fatalf("read: %v", err)
	}
	return strings.TrimRight(l, "\n")
}

// send writes one command with a fresh id and returns the verb of its answer.
func (s *varsSession) send(format string, args ...any) []string {
	s.t.Helper()
	s.n++
	fmt.Fprintf(s.conn, format+"\n", append([]any{s.n}, args...)...)
	return strings.Split(s.line(), "\t")
}

// plain runs a PLAIN login; extra is the AUTH fields after resp.
func (s *varsSession) plain(authzid, user, pw, extra string) string {
	resp := base64.StdEncoding.EncodeToString([]byte(authzid + "\x00" + user + "\x00" + pw))
	return s.send("AUTH\t%d\tPLAIN\tresp=%s\t%s", resp, extra)[0]
}

// scram runs a SCRAM-SHA-256 exchange for alice over service.
func (s *varsSession) scram(service string) string {
	s.t.Helper()
	d := newScramClientDriver(MechScramSha256, "alice", "pw", nil)
	f := s.send("AUTH\t%d\t%s\tservice=%s\tsession=s1\tresp=%s", MechScramSha256, service,
		base64.StdEncoding.EncodeToString(d.clientFirst()))
	if f[0] != "CONT" {
		return f[0]
	}
	first, _ := base64.StdEncoding.DecodeString(f[2])
	fmt.Fprintf(s.conn, "CONT\t%d\t%s\n", s.n, base64.StdEncoding.EncodeToString(d.clientFinal(s.t, first)))
	return strings.Split(s.line(), "\t")[0]
}

// oauth runs an OAUTHBEARER login for alice over service.
func (s *varsSession) oauth(service string) string {
	initial := base64.StdEncoding.EncodeToString([]byte("n,a=alice,\x01auth=Bearer good-token\x01\x01"))
	f := s.send("AUTH\t%d\t%s\tservice=%s\tresp=%s", MechOAuthBearer, service, initial)
	if f[0] == "CONT" {
		fmt.Fprintf(s.conn, "CONT\t%d\t%s\n", s.n, base64.StdEncoding.EncodeToString([]byte{1}))
		return strings.Split(s.line(), "\t")[0]
	}
	return f[0]
}

func scramCreds(t *testing.T) *sasl.ScramCredentials {
	t.Helper()
	c, err := sasl.GenerateScramSha256Credentials("pw", sasl.MinScramIterations)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A protocol rule holds on every way in: the password query's on the SCRAM
// verifier lookup, the user query's on every login it admitted (#2227).
func TestAProtocolRuleHoldsOnEveryLoginPath(t *testing.T) {
	imapOff := map[string]bool{"imap": true}
	rows := []struct {
		name  string
		srv   func(t *testing.T) *Server
		login func(s *varsSession, service string) string
	}{
		{"SCRAM, rule in the password query", func(t *testing.T) *Server {
			return NewServer([]Passdb{ruledSCRAM{creds: scramCreds(t), refuse: imapOff}})
		}, (*varsSession).scram},
		{"SCRAM, rule in the user query", func(t *testing.T) *Server {
			return NewServer([]Passdb{ruledSCRAM{creds: scramCreds(t)}}, WithUserdb(ruledUserdb{imapOff}))
		}, (*varsSession).scram},
		{"password, rule in the user query", func(*testing.T) *Server {
			return NewServer([]Passdb{&credPassdb{"alice", "pw"}}, WithUserdb(ruledUserdb{imapOff}))
		}, func(s *varsSession, service string) string { return s.plain("", "alice", "pw", "service="+service) }},
		{"OAUTHBEARER, rule in the user query", func(*testing.T) *Server {
			return NewServer([]Passdb{tokenPassdb{}}, WithUserdb(ruledUserdb{imapOff}))
		}, (*varsSession).oauth},
		{"master user, rule in the user query", func(*testing.T) *Server {
			return NewServer([]Passdb{&credPassdb{"alice", "pw"}}, WithUserdb(ruledUserdb{imapOff}),
				WithMasterUsers(true), WithMasterdb([]Passdb{&credPassdb{"admin", "mpw"}}))
		}, func(s *varsSession, service string) string {
			return s.plain("alice", "admin", "mpw", "service="+service)
		}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			s := serveVars(t, r.srv(t))
			if got := r.login(s, "imap"); got != "FAIL" {
				t.Errorf("imap, refused by the rule: answered %s", got)
			}
			if got := r.login(s, "pop3"); got != "OK" {
				t.Errorf("pop3, not ruled: answered %s", got)
			}
		})
	}
}

// A refusal by rule is a wrong password's answer, its delay included (D4).
func TestARuleRefusalAnswersAsAWrongPassword(t *testing.T) {
	const delay = 150 * time.Millisecond
	s := serveVars(t, NewServer([]Passdb{&credPassdb{"alice", "pw"}},
		WithUserdb(ruledUserdb{map[string]bool{"imap": true}}), WithFailureDelay(delay)))
	timed := func(pw string) (string, time.Duration) {
		start := time.Now()
		resp := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00" + pw))
		f := s.send("AUTH\t%d\tPLAIN\tresp=%s\tservice=imap", resp)
		f[1] = "" // the id differs by construction
		return strings.Join(f, "\t"), time.Since(start)
	}
	wrong, wrongTook := timed("bad")
	ruled, ruledTook := timed("pw")
	if ruled != wrong {
		t.Errorf("refused by rule %q, wrong password %q", ruled, wrong)
	}
	if ruledTook < delay || wrongTook < delay {
		t.Errorf("delays: rule %v, wrong password %v; want both >= %v", ruledTook, wrongTook, delay)
	}
}

// With a passdb reading the addresses, an answer cached for one address does
// not stand for another (#2227).
func TestTheCacheIsKeyedByTheAddressesAPassdbReads(t *testing.T) {
	s := serveVars(t, NewServer([]Passdb{addressPassdb{}}, WithCache(NewCache(1<<20, time.Minute, time.Minute))))
	rows := []struct{ extra, want string }{
		{"service=imap\trip=10.0.0.1\tlip=10.0.0.2", "OK"},
		{"service=imap\trip=10.9.9.9\tlip=10.0.0.2", "FAIL"},
		{"service=imap\trip=10.0.0.1\tlip=10.8.8.8", "FAIL"},
	}
	for _, r := range rows {
		if got := s.plain("", "alice", "pw", r.extra); got != r.want {
			t.Errorf("%s: answered %s, want %s", r.extra, got, r.want)
		}
	}
}

// USER carries the protocol of a login or a delivery; a lookup naming none
// is neither and the rule's ELSE lets it through (#2227).
func TestUSERCarriesTheProtocol(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go NewMasterServer(ruledUserdb{map[string]bool{"lmtp": true}}).Serve(ctx, ln) //nolint:errcheck
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })                //nolint:errcheck
	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	rd := bufio.NewReader(conn)
	for {
		l, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == "DONE\n" {
			break
		}
	}
	for i, r := range []struct{ fields, want string }{
		{"\tservice=lmtp", "NOTFOUND"},
		{"\tservice=imap", "USER"},
		{"", "USER"},
	} {
		fmt.Fprintf(conn, "USER\t%d\talice%s\n", i+1, r.fields)
		l, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.SplitN(l, "\t", 2)[0]; got != r.want {
			t.Errorf("USER%q: answered %s, want %s", r.fields, got, r.want)
		}
	}
}
