package protocol

import (
	"encoding/base64"
	"fmt"
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
