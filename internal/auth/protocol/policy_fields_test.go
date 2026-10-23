package protocol

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// knownPassdb knows one user: a wrong password fails, anyone else is passed on.
type knownPassdb struct{ user, pass string }

func (k *knownPassdb) Authenticate(req *Request) (Result, error) {
	switch {
	case req.Username != k.user:
		return ResultNext, nil
	case req.Password != k.pass:
		return ResultFail, nil
	}
	req.Fields.Set("user", req.Username)
	return ResultOK, nil
}

// waitReports waits for the asynchronous reports and returns them.
func (p *stubPolicy) waitReports(t *testing.T, n int) []reportCall {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		got := append([]reportCall(nil), p.reportCalls...)
		p.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// policyServer is a server with the stub policy on every hook and the tarpit
// recorded rather than slept.
func policyServer(t *testing.T, p *stubPolicy, passdbs []Passdb, opts ...ServerOption) (*Server, *[]time.Duration) {
	t.Helper()
	srv := NewServer(passdbs, append([]ServerOption{
		WithPolicy(p, PolicyMode{CheckBefore: true, CheckAfter: true, ReportAfter: true}),
	}, opts...)...)
	var mu sync.Mutex
	slept := &[]time.Duration{}
	srv.sleep = func(d time.Duration) { mu.Lock(); *slept = append(*slept, d); mu.Unlock() }
	return srv, slept
}

func authLine(t *testing.T, srv *Server, line string) string {
	t.Helper()
	addr, cancel := startTestServer(t, srv)
	t.Cleanup(cancel)
	conn, sc := dialAndHandshake(t, addr)
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck
	fmt.Fprintln(conn, line)
	if !sc.Scan() {
		t.Fatal("no reply")
	}
	return sc.Text()
}

// plainResp is the resp= field of a PLAIN login.
func plainResp(user, pass string) string {
	return "resp=" + base64.StdEncoding.EncodeToString([]byte("\x00"+user+"\x00"+pass))
}

var alicePlain = plainResp("alice", "secret")

func TestThePolicyHearsTheClientConnection(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		check  func(r PolicyRequest) string
	}{
		{"session_id is the login's session", "\tsession=abc123", func(r PolicyRequest) string {
			if r.SessionID != "abc123" {
				return fmt.Sprintf("SessionID %q", r.SessionID)
			}
			return ""
		}},
		{"lip is the local address", "\tlip=198.51.100.7", func(r PolicyRequest) string {
			if r.LocalIP != "198.51.100.7" {
				return fmt.Sprintf("LocalIP %q", r.LocalIP)
			}
			return ""
		}},
		{"secured=tls is TLS", "\tsecured=tls", func(r PolicyRequest) string {
			if !r.TLS {
				return "TLS false"
			}
			return ""
		}},
		{"secured without TLS is not TLS", "\tsecured", func(r PolicyRequest) string {
			if r.TLS {
				return "TLS true"
			}
			return ""
		}},
		{"client_id is the device", "\tclient_id=\"name\" \"x\x01ty\"", func(r PolicyRequest) string {
			if r.DeviceID != "\"name\" \"x\ty\"" {
				return fmt.Sprintf("DeviceID %q", r.DeviceID)
			}
			return ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubPolicy{beforeDecision: PolicyDecision{Continue: true}, afterDecision: PolicyDecision{Continue: true}}
			srv, _ := policyServer(t, p, []Passdb{&knownPassdb{"alice", "secret"}})
			if got := authLine(t, srv, "AUTH\t1\tPLAIN\tservice=imap\t"+alicePlain+tc.fields); !strings.HasPrefix(got, "OK\t") {
				t.Fatalf("reply %q", got)
			}
			if len(p.beforeCalls) != 1 {
				t.Fatalf("%d before calls", len(p.beforeCalls))
			}
			for _, r := range []PolicyRequest{p.beforeCalls[0], p.afterCalls[0].req, p.waitReports(t, 1)[0].req} {
				if msg := tc.check(r); msg != "" {
					t.Error(msg)
				}
			}
		})
	}
}

func TestAFailureIsReportedWithItsType(t *testing.T) {
	cases := []struct {
		name, resp, want string
	}{
		{"a wrong password", plainResp("alice", "wrong"), "credentials"},
		{"an unknown account", plainResp("bob", "secret"), "account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubPolicy{beforeDecision: PolicyDecision{Continue: true}}
			srv, _ := policyServer(t, p, []Passdb{&knownPassdb{"alice", "secret"}})
			authLine(t, srv, "AUTH\t1\tPLAIN\tservice=imap\t"+tc.resp)
			got := p.waitReports(t, 1)
			if len(got) != 1 || got[0].req.FailType != tc.want {
				t.Fatalf("reports %+v, want one with fail_type %s", got, tc.want)
			}
		})
	}
}

// A refusal answered from the negative cache keeps its type.
func TestACachedFailureKeepsItsType(t *testing.T) {
	p := &stubPolicy{beforeDecision: PolicyDecision{Continue: true}}
	srv, _ := policyServer(t, p, []Passdb{&knownPassdb{"alice", "secret"}},
		WithCache(NewCache(1<<20, time.Minute, time.Minute)))
	addr, cancel := startTestServer(t, srv)
	defer cancel()
	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close() //nolint:errcheck
	for id := 1; id <= 2; id++ {
		fmt.Fprintf(conn, "AUTH\t%d\tPLAIN\tservice=imap\t%s\n", id, plainResp("bob", "secret"))
		sc.Scan()
	}
	got := p.waitReports(t, 2)
	if len(got) != 2 || got[1].req.FailType != "account" {
		t.Fatalf("reports %+v, want the cached one with fail_type account", got)
	}
}

func TestARefusalCarriesItsMessage(t *testing.T) {
	p := &stubPolicy{beforeDecision: PolicyDecision{Reject: true, Message: "Too many\tfailed logins"}}
	srv, _ := policyServer(t, p, []Passdb{&knownPassdb{"alice", "secret"}})
	if got := authLine(t, srv, "AUTH\t1\tPLAIN\tservice=imap\t"+alicePlain); got != "FAIL\t1\treason=Too many\x01tfailed logins" {
		t.Errorf("reply %q", got)
	}
	if r := p.waitReports(t, 1); len(r) != 1 || r[0].req.FailType != "policy" || !r[0].reject {
		t.Errorf("reports %+v, want one policy refusal", r)
	}
}

func TestOnlyTheCheckBeforeTarpits(t *testing.T) {
	cases := []struct {
		name          string
		before, after int
		noPenalty     bool
		want          []time.Duration
	}{
		{"before the exchange", 3, 0, false, []time.Duration{3 * time.Second}},
		{"not after the check of a success", 0, 3, false, nil},
		{"not for a client without penalty", 3, 0, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubPolicy{
				beforeDecision: PolicyDecision{Continue: true, TarpitSecs: tc.before},
				afterDecision:  PolicyDecision{Continue: true, TarpitSecs: tc.after},
			}
			srv, slept := policyServer(t, p, []Passdb{&knownPassdb{"alice", "secret"}})
			line := "AUTH\t1\tPLAIN\tservice=imap\t" + alicePlain
			if tc.noPenalty {
				line += "\tno-penalty"
			}
			if got := authLine(t, srv, line); !strings.HasPrefix(got, "OK\t") {
				t.Fatalf("reply %q", got)
			}
			if fmt.Sprint(*slept) != fmt.Sprint(tc.want) {
				t.Errorf("tarpits %v, want %v", *slept, tc.want)
			}
		})
	}
}

func TestPolicyDecisionsAreCounted(t *testing.T) {
	cases := []struct {
		name     string
		decision PolicyDecision
		result   string
	}{
		{"allowed", PolicyDecision{Continue: true}, "ok"},
		{"tarpitted", PolicyDecision{Continue: true, TarpitSecs: 1}, "delayed"},
		{"refused", PolicyDecision{Reject: true}, "refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := testutil.ToFloat64(policyDecisions.WithLabelValues("before", tc.result))
			p := &stubPolicy{beforeDecision: tc.decision, afterDecision: PolicyDecision{Continue: true}}
			srv, _ := policyServer(t, p, []Passdb{&knownPassdb{"alice", "secret"}})
			authLine(t, srv, "AUTH\t1\tPLAIN\tservice=imap\t"+alicePlain)
			if got := testutil.ToFloat64(policyDecisions.WithLabelValues("before", tc.result)) - before; got != 1 {
				t.Errorf("before/%s counted %v, want 1", tc.result, got)
			}
		})
	}
}

// SCRAM is checked where its credentials are looked up and reported after.
func TestAScramLoginGoesThroughThePolicy(t *testing.T) {
	cases := []struct {
		name       string
		user, pass string
		before     PolicyDecision
		wantReply  string
		wantReport string // fail_type; "ok" for a success
	}{
		{"a success is checked and reported", "alice", "secret", PolicyDecision{Continue: true}, "OK\t", "ok"},
		{"a wrong password is reported as credentials", "alice", "wrong", PolicyDecision{Continue: true}, "FAIL\t", "credentials"},
		{"an unknown user is reported as account", "bob", "secret", PolicyDecision{Continue: true}, "FAIL\t", "account"},
		{"a refusal stops it with the message", "alice", "secret", PolicyDecision{Reject: true, Message: "go away"}, "FAIL\t1\treason=go away", "policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubPolicy{beforeDecision: tc.before, afterDecision: PolicyDecision{Continue: true}}
			srv, _ := policyServer(t, p, scramChain(t, "alice", "secret"))
			got := runSCRAM(t, srv, MechScramSha256, tc.user, tc.pass, nil)
			if !strings.HasPrefix(got, tc.wantReply) {
				t.Fatalf("reply %q, want %q", got, tc.wantReply)
			}
			p.mu.Lock()
			checked := len(p.beforeCalls) == 1 && p.beforeCalls[0].Username == tc.user
			p.mu.Unlock()
			if !checked {
				t.Errorf("before calls %+v, want one for %s", p.beforeCalls, tc.user)
			}
			r := p.waitReports(t, 1)
			if len(r) != 1 {
				t.Fatalf("%d reports", len(r))
			}
			if tc.wantReport == "ok" {
				if !r[0].success {
					t.Errorf("report %+v, want a success", r[0])
				}
			} else if r[0].success || r[0].req.FailType != tc.wantReport {
				t.Errorf("report %+v, want a failure with fail_type %s", r[0], tc.wantReport)
			}
		})
	}
}

// A SCRAM exchange tells the policy what the AUTH line told the service.
func TestAScramExchangeCarriesItsConnection(t *testing.T) {
	x := &saslExchange{username: "alice", rip: "192.0.2.1", lip: "198.51.100.2", service: "imap", session: "s1",
		client: clientConn{secured: "tls", clientID: `"name" "x"`}}
	want := PolicyRequest{Username: "alice", RemoteIP: "192.0.2.1", LocalIP: "198.51.100.2", Service: "imap",
		SessionID: "s1", TLS: true, DeviceID: `"name" "x"`}
	got := x.policyRequest()
	vars := got.Vars
	got.Vars = nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("policyRequest = %+v, want %+v", got, want)
	}
	if vars["local_ip"] != "198.51.100.2" || vars["secured"] != "TLS" || vars["client_id"] != `"name" "x"` {
		t.Errorf("vars %v", vars)
	}
}

// fieldsPassdb is a second passdb that knows alice and leaves fields.
type fieldsPassdb struct{}

func (fieldsPassdb) Authenticate(req *Request) (Result, error) {
	if req.Username != "alice@example.com" || req.Password != "secret" {
		return ResultNext, nil
	}
	req.Fields.Set("user", "alice@example.com")
	req.Fields.Set("tenant", "acme")
	req.Fields.Set("userdb_quota_rule", "*:storage=1G")
	return ResultOK, nil
}

// The variable table holds what the AUTH line and the connection said, and
// after the chain which passdb answered and the fields it left.
func TestThePolicyHearsTheVariableTable(t *testing.T) {
	p := &stubPolicy{beforeDecision: PolicyDecision{Continue: true}, afterDecision: PolicyDecision{Continue: true}}
	srv, _ := policyServer(t, p, []Passdb{&knownPassdb{"bob", "x"}, fieldsPassdb{}})
	addr, cancel := startTestServer(t, srv)
	defer cancel()
	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close() //nolint:errcheck
	fmt.Fprint(conn, "CPID\t4242\n")
	fmt.Fprintf(conn, "AUTH\t1\tPLAIN\tservice=managesieve\t%s\trip=192.0.2.1\tlip=198.51.100.2\tsession=s1"+
		"\tsecured=tls\tlport=4190\trport=51000\treal_rip=10.0.0.5\treal_rport=33000\tlocal_name=mail.example.org"+
		"\tssl_ja3_hash=0123456789abcdef0123456789abcdef\tvalid-client-cert\n", plainResp("alice@example.com", "secret"))
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "OK\t") {
		t.Fatalf("reply %q", sc.Text())
	}
	p.mu.Lock()
	before, after := p.beforeCalls[0].Vars, p.afterCalls[0].req
	p.mu.Unlock()
	for _, tc := range []struct{ name, want string }{
		{"user", "alice@example.com"}, {"protocol", "sieve"}, {"mechanism", "PLAIN"}, {"secured", "TLS"},
		{"client_pid", "4242"}, {"remote_ip", "192.0.2.1"}, {"local_ip", "198.51.100.2"},
		{"remote_port", "51000"}, {"local_port", "4190"}, {"real_remote_ip", "10.0.0.5"},
		{"real_local_ip", "198.51.100.2"}, {"real_remote_port", "33000"}, {"real_local_port", "4190"},
		{"local_name", "mail.example.org"}, {"ssl_ja3_hash", "0123456789abcdef0123456789abcdef"},
		{"cert", "valid"}, {"session", "s1"}, {"password", "secret"}, {"domain_first", "example.com"},
		{"domain_last", "example.com"}, {"original_user", "alice@example.com"}, {"auth_user", "alice@example.com"},
		{"owner_user", "alice@example.com"}, {"id", ""},
	} {
		if got := before[tc.name]; got != tc.want {
			t.Errorf("before: %s = %q, want %q", tc.name, got, tc.want)
		}
	}
	if after.Vars["id"] != "2" {
		t.Errorf("after: id = %q, want 2, the second passdb", after.Vars["id"])
	}
	if after.Passdb["tenant"] != "acme" || after.Userdb["quota_rule"] != "*:storage=1G" {
		t.Errorf("after: passdb %v, userdb %v", after.Passdb, after.Userdb)
	}
}

// Without real_ fields the real address is the plain one, and unset ports
// read 0, as the reference's table reads them.
func TestRealAddressesFallBack(t *testing.T) {
	v := policyVars{rip: "192.0.2.1", lip: "198.51.100.2"}.table()
	for _, tc := range []struct{ name, want string }{
		{"real_remote_ip", "192.0.2.1"}, {"real_local_ip", "198.51.100.2"},
		{"remote_port", "0"}, {"real_remote_port", "0"}, {"domain_first", ""},
	} {
		if v[tc.name] != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, v[tc.name], tc.want)
		}
	}
	if d := (policyVars{user: "a@b@c"}).table(); d["domain_first"] != "b" || d["domain_last"] != "c" {
		t.Errorf("a@b@c: domain_first %q, domain_last %q, want b and c", d["domain_first"], d["domain_last"])
	}
}
