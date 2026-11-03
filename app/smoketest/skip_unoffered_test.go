package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/sieve"
)

// A sub-check names only extensions the server can offer, or a misspelt one
// would skip it on every deployment.
func TestSieveTestsNeedSupportedExtensions(t *testing.T) {
	for _, st := range sieveTests {
		for _, e := range st.needs {
			if !slices.Contains(sieve.SupportedExtensions, e) {
				t.Errorf("%s needs %q, which no server offers", st.name, e)
			}
		}
	}
}

// An extension the server does not offer skips its sub-check; a failure of an
// offered one still fails the row, and nothing offered skips the row (#2473).
func TestSieveSubChecksFollowTheOffer(t *testing.T) {
	failing := errors.New("broken")
	tests := []struct {
		name    string
		offered []string
		wantRan []string
		wantErr string
	}{
		{"pipe not offered", []string{"fileinto"}, []string{"fileinto", "plain"}, ""},
		{"pipe offered and broken", []string{"fileinto", "vnd.yarilo.pipe"}, []string{"fileinto", "pipe", "plain"}, "pipe: broken"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ran []string
			mk := func(name string, err error) func(string, string, string) error {
				return func(string, string, string) error { ran = append(ran, name); return err }
			}
			subs := []sieveTest{
				{"fileinto", []string{"fileinto"}, mk("fileinto", nil)},
				{"pipe", []string{"vnd.yarilo.pipe"}, mk("pipe", failing)},
				{"plain", nil, mk("plain", nil)},
			}
			offered := map[string]bool{}
			for _, e := range tc.offered {
				offered[e] = true
			}
			err := runSieveTests(subs, offered, "u", "p", "u")
			if !slices.Equal(ran, tc.wantRan) {
				t.Errorf("ran %v, want %v", ran, tc.wantRan)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v, want none", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	t.Run("nothing offered", func(t *testing.T) {
		subs := []sieveTest{{"pipe", []string{"vnd.yarilo.pipe"}, func(string, string, string) error { return nil }}}
		var u unmeasurableError
		if err := runSieveTests(subs, map[string]bool{}, "u", "p", "u"); !errors.As(err, &u) {
			t.Errorf("err = %v, want a skip", err)
		}
	})
}

// fakeIMAPCaps is an IMAP server announcing caps; MYRIGHTS is refused so a
// row that gets past the capability fails rather than passes.
func fakeIMAPCaps(t *testing.T, caps string) {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() //nolint:errcheck
				r := bufio.NewReader(conn)
				fmt.Fprintf(conn, "* OK fake\r\n") //nolint:errcheck
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					tag, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
					verb, _, _ := strings.Cut(rest, " ")
					switch strings.ToUpper(verb) {
					case "CAPABILITY":
						fmt.Fprintf(conn, "* CAPABILITY %s\r\n%s OK done\r\n", caps, tag) //nolint:errcheck
					case "MYRIGHTS", "CREATE":
						fmt.Fprintf(conn, "%s NO refused\r\n", tag) //nolint:errcheck
					default:
						fmt.Fprintf(conn, "%s OK done\r\n", tag) //nolint:errcheck
					}
				}
			}()
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	for _, f := range []struct {
		p *string
		v string
	}{{flagHost, host}, {flagIMAPHost, ""}, {flagIMAPSPort, port}, {flagIMAPTLS, "none"}} {
		old := *f.p
		*f.p = f.v
		t.Cleanup(func() { *f.p = old })
	}
}

// Both ACL rows skip on a server without ACL in CAPABILITY and fail on one
// that offers ACL and then refuses it (#2473).
func TestACLRowsFollowTheCapability(t *testing.T) {
	rows := map[string]func() error{
		"acl":        func() error { return checkACL("u", "p") },
		"disclosure": func() error { return checkACLDisclosure("u", "p", "peer", "p", "Public/") },
	}
	for name, row := range rows {
		for _, tc := range []struct {
			caps     string
			wantSkip bool
		}{{"IMAP4rev2 QUOTA", true}, {"IMAP4rev2 ACL QUOTA", false}} {
			t.Run(name+"/"+tc.caps, func(t *testing.T) {
				fakeIMAPCaps(t, tc.caps)
				err := row()
				var u unmeasurableError
				if skipped := errors.As(err, &u); skipped != tc.wantSkip || err == nil {
					t.Fatalf("err = %v, want skip %v and an error either way", err, tc.wantSkip)
				}
			})
		}
	}
}

// The ticket row skips when the account does not offer tickets, and fails
// when it offers them without an endpoint to get one from (#2473).
func TestTicketRowFollowsTheSession(t *testing.T) {
	const acc = `"primaryAccounts":{"urn:ietf:params:jmap:mail":"a1"}`
	tests := []struct {
		name     string
		session  string
		wantSkip bool
		wantPath string
	}{
		{"not offered", `{` + acc + `,"capabilities":{},"accounts":{"a1":{"accountCapabilities":{"urn:ietf:params:jmap:websocket":{}}}}}`, true, ""},
		{"offered without an endpoint", `{` + acc + `,"capabilities":{"` + capWebSocketTicket + `":{}},"accounts":{"a1":{"accountCapabilities":{"` + capWebSocketTicket + `":{},"urn:ietf:params:jmap:websocket":{}}}}}`, false, ""},
		{"offered", `{` + acc + `,"capabilities":{"` + capWebSocketTicket + `":{"generationEndpoint":"https://x/jmap/ws/ticket"}},"accounts":{"a1":{"accountCapabilities":{"` + capWebSocketTicket + `":{},"urn:ietf:params:jmap:websocket":{}}}}}`, false, "/jmap/ws/ticket"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.session) //nolint:errcheck
			}))
			t.Cleanup(ts.Close)
			host, port, _ := net.SplitHostPort(ts.Listener.Addr().String())
			oldAcc := jmapAccount
			jmapAccount = &accountCache{}
			t.Cleanup(func() { jmapAccount = oldAcc })
			for _, f := range []struct {
				p *string
				v string
			}{{flagJMAPHost, host}, {flagJMAPPort, port}} {
				old := *f.p
				*f.p = f.v
				t.Cleanup(func() { *f.p = old })
			}
			oldInsecure := *flagInsecure
			*flagInsecure = true
			t.Cleanup(func() { *flagInsecure = oldInsecure })

			path, err := ticketEndpoint("https://" + net.JoinHostPort(host, port))
			var u unmeasurableError
			switch {
			case tc.wantSkip && !errors.As(err, &u):
				t.Fatalf("err = %v, want a skip", err)
			case !tc.wantSkip && tc.wantPath == "" && (err == nil || errors.As(err, &u)):
				t.Fatalf("err = %v, want a failure", err)
			case tc.wantPath != "" && (err != nil || path != tc.wantPath):
				t.Fatalf("path = %q, err = %v; want %q", path, err, tc.wantPath)
			}
		})
	}
}

// The server's refusal of the text condition is a skip; any other refusal
// is not (#2473).
func TestTextNotOffered(t *testing.T) {
	tests := []struct {
		errType, desc string
		want          bool
	}{
		{"unsupportedFilter", "conditions not supported: text", true},
		{"unsupportedFilter", "conditions not supported yet: body, text", true},
		{"unsupportedFilter", "conditions not supported: body", false},
		{"unsupportedFilter", "filter operators are not supported yet", false},
		{"invalidArguments", "conditions not supported: text", false},
	}
	for _, tc := range tests {
		if got := textNotOffered(tc.errType, tc.desc); got != tc.want {
			t.Errorf("textNotOffered(%q, %q) = %v, want %v", tc.errType, tc.desc, got, tc.want)
		}
	}
}

// The folder the round trip creates is one level under either separator.
func TestRoundTripMailboxNameHasNoSeparator(t *testing.T) {
	stamp := time.Date(2026, 11, 2, 9, 0, 0, 123456789, time.UTC).Format("20060102T150405.000000000")
	name := roundTripMailboxName(stamp)
	for _, sep := range []string{".", "/"} {
		if strings.Contains(name, sep) {
			t.Errorf("%q splits under the separator %q", name, sep)
		}
	}
	if name != "smoketest-20261102T090000123456789" {
		t.Errorf("name = %q", name)
	}
}

// Ten requests in a row reuse one connection; a client per request left ten
// idle behind and filled a JMAP limit of 10 (#2472).
func TestJMAPClientReusesItsConnection(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	ts.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	host, _, _ := net.SplitHostPort(ts.Listener.Addr().String())
	oldHost, oldInsecure := *flagJMAPHost, *flagInsecure
	*flagJMAPHost, *flagInsecure = host, true
	t.Cleanup(func() { *flagJMAPHost, *flagInsecure = oldHost, oldInsecure })

	for i := 0; i < 10; i++ {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := jmapClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close() //nolint:errcheck
	}
	mu.Lock()
	defer mu.Unlock()
	if conns != 1 {
		t.Errorf("10 requests opened %d connections, want 1", conns)
	}
}
