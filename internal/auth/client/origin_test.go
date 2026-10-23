package client

import (
	"errors"
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
