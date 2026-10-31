package client

import (
	"bufio"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What the login knows about the client's connection reaches the AUTH line in
// the form the service reads.
func TestTheAuthLineCarriesTheClientConnection(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
		want []string
		not  []string
	}{
		{"nothing known", nil, nil, []string{"\tsecured", "\tclient_id=", "\tno-penalty"}},
		{"TLS", []Option{TLS()}, []string{"\tsecured=tls"}, nil},
		{"a client ID, escaped", []Option{ClientID("\"name\" \"a\tb\"")}, []string{"\tclient_id=\"name\" \"a\x01tb\""}, nil},
		{"no penalty", []Option{NoPenalty()}, []string{"\tno-penalty"}, nil},
		{"ports and the real peer", []Option{Conn(Addresses{LocalPort: 993, RemotePort: 51000, RealLocalIP: "10.0.0.1",
			RealRemoteIP: "10.0.0.5", RealLocalPort: 993, RealRemotePort: 33000})},
			[]string{"\tlport=993", "\trport=51000", "\treal_lip=10.0.0.1", "\treal_rip=10.0.0.5", "\treal_lport=993", "\treal_rport=33000"}, nil},
		{"no ports when unknown", []Option{Conn(Addresses{})}, nil, []string{"\tlport=", "\trport=", "\treal_"}},
		{"the TLS server name", []Option{LocalName("mail.example.org")}, []string{"\tlocal_name=mail.example.org"}, nil},
		{"the hello fingerprint", []Option{JA3Hash("0123")}, []string{"\tssl_ja3_hash=0123"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan string, 1)
			c, err := New(serveOneAuthLine(t, got), nil, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close() //nolint:errcheck
			_, _ = c.AuthenticateAs("", "u1", "secret", "imap", "10.0.0.1", "", "sess1", tc.opts...)
			var line string
			select {
			case line = <-got:
			case <-time.After(2 * time.Second):
				t.Fatal("no request reached the server")
			}
			for _, w := range tc.want {
				if !strings.Contains(line, w) {
					t.Errorf("%q lacks %q", line, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(line, n) {
					t.Errorf("%q carries %q", line, n)
				}
			}
		})
	}
}

func TestAFailWithAReasonIsARefusalForTheClient(t *testing.T) {
	cases := []struct {
		line, reason string
		temp         bool
	}{
		{"FAIL\t1", "", false},
		{"FAIL\t1\treason=Too many\x01tfailed logins", "Too many\tfailed logins", false},
		{"FAIL\t1\tcode=temp_fail", "", true},
	}
	for _, tc := range cases {
		_, err := parseAuthResponse(tc.line)
		if tc.temp {
			if !errors.Is(err, ErrTempFail) {
				t.Errorf("%q: %v, want ErrTempFail", tc.line, err)
			}
			continue
		}
		if !errors.Is(err, ErrAuthFailed) {
			t.Errorf("%q: %v is not ErrAuthFailed", tc.line, err)
		}
		if got := RefusalReason(err); got != tc.reason {
			t.Errorf("%q: reason %q, want %q", tc.line, got, tc.reason)
		}
	}
}

// The handshake names this process, as the service's client_pid.
func TestTheHandshakeSendsTheClientPID(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		r := bufio.NewReader(conn)
		_, _ = conn.Write([]byte("VERSION\t1\t0\nDONE\n"))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "CPID\t") {
				got <- strings.TrimSpace(line)
				return
			}
		}
	}()
	c, err := New(ln.Addr().String(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck
	go func() { _, _ = c.LookupUser("u") }()
	select {
	case line := <-got:
		if line != "CPID\t"+strconv.Itoa(os.Getpid()) {
			t.Errorf("handshake sent %q, want CPID with this pid", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no CPID in the handshake")
	}
}
