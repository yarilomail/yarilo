package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

// fakeMX answers EHLO with the given capability lines, so what the inbound
// listener offers is an input of the test rather than a property of the stand.
func fakeMX(t *testing.T, caps ...string) (host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
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
				fmt.Fprintf(conn, "220 fake-mx ESMTP\r\n") //nolint:errcheck
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if !strings.HasPrefix(strings.ToUpper(line), "EHLO") {
						fmt.Fprintf(conn, "221 bye\r\n") //nolint:errcheck
						return
					}
					for _, c := range caps {
						fmt.Fprintf(conn, "250-%s\r\n", c) //nolint:errcheck
					}
					fmt.Fprintf(conn, "250 CHUNKING\r\n") //nolint:errcheck
				}
			}()
		}
	}()
	host, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return host, port
}

func pointMXAt(t *testing.T, host, port string) {
	t.Helper()
	oldHost, oldPort := *flagSMTPMXHost, *flagSMTPMXPort
	*flagSMTPMXHost, *flagSMTPMXPort = host, port
	t.Cleanup(func() { *flagSMTPMXHost, *flagSMTPMXPort = oldHost, oldPort })
}

// An inbound listener offers TLS and no login: a row that only counts a 250
// passes against submission, against a relay, against anything that greets.
func TestTheMXRowReadsTheShapeOfTheInboundListener(t *testing.T) {
	rows := []struct {
		name    string
		caps    []string
		wantErr string
	}{
		{"an MX", []string{"PIPELINING", "SIZE 10240000", "STARTTLS", "8BITMIME"}, ""},
		{"no TLS offered", []string{"PIPELINING", "SIZE 10240000"}, "STARTTLS"},
		{"login in the clear", []string{"STARTTLS", "AUTH PLAIN LOGIN"}, "AUTH"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			host, port := fakeMX(t, row.caps...)
			pointMXAt(t, host, port)
			err := checkSMTPMX()
			if row.wantErr == "" {
				if err != nil {
					t.Fatalf("checkSMTPMX: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkSMTPMX accepted a listener advertising %v", row.caps)
			}
			if !strings.Contains(err.Error(), row.wantErr) {
				t.Errorf("checkSMTPMX error %v does not name %q", err, row.wantErr)
			}
		})
	}
}

// The MX host is its own flag: the inbound listener runs outside this
// release's namespace, so it is not the submission host.
func TestTheMXHostFallsBackToTheSMTPHostThenTheHost(t *testing.T) {
	oldMX, oldSMTP, oldHost := *flagSMTPMXHost, *flagSMTPHost, *flagHost
	t.Cleanup(func() { *flagSMTPMXHost, *flagSMTPHost, *flagHost = oldMX, oldSMTP, oldHost })

	*flagSMTPMXHost, *flagSMTPHost, *flagHost = "", "", "plain-host"
	if got := mxHost(); got != "plain-host" {
		t.Errorf("mxHost() = %q, want the -host fallback", got)
	}
	*flagSMTPHost = "submission-host"
	if got := mxHost(); got != "submission-host" {
		t.Errorf("mxHost() = %q, want the -smtp-host fallback", got)
	}
	*flagSMTPMXHost = "mx-host"
	if got := mxHost(); got != "mx-host" {
		t.Errorf("mxHost() = %q, want the -smtp-mx-host value", got)
	}
}

// The probe's sender lives in the recipient's domain: an MX refuses a sender
// whose domain does not resolve, and test.invalid never does.
func TestTheProxyProbeSenderSharesTheRecipientDomain(t *testing.T) {
	rows := []struct{ rcpt, want string }{
		{"u1@d00001.test", "proxy-probe@d00001.test"},
		{"user@sub.example.org", "proxy-probe@sub.example.org"},
		{"no-at-sign", "no-at-sign"},
	}
	for _, row := range rows {
		if got := proxyProbeSender(row.rcpt); got != row.want {
			t.Errorf("proxyProbeSender(%q) = %q, want %q", row.rcpt, got, row.want)
		}
	}
}
