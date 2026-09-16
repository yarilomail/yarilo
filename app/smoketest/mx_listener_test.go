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

// fakeProxyReader answers like a PROXY listener that parses: it reads the
// header, refuses one whose fields are not addresses, then speaks SMTP.
func fakeProxyReader(t *testing.T) (host, port string, got chan string) {
	t.Helper()
	got = make(chan string, 4)
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
				header, err := r.ReadString('\n')
				if err != nil {
					return
				}
				got <- strings.TrimRight(header, "\r\n")
				fields := strings.Fields(header)
				if len(fields) < 6 || net.ParseIP(fields[2]) == nil || net.ParseIP(fields[3]) == nil {
					return // the parser hangs up, as Postfix does
				}
				fmt.Fprintf(conn, "220 fake-proxy ESMTP\r\n") //nolint:errcheck
			}()
		}
	}()
	host, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return host, port, got
}

// The header carries addresses in both fields even when the listener was named
// by hostname: a parser refuses anything else and the row reddens over nothing.
func TestTheProxyHeaderCarriesAddressesNotNames(t *testing.T) {
	host, port, got := fakeProxyReader(t)

	oldMX, oldPort, oldProxyPort := *flagSMTPMXHost, *flagSMTPMXPort, *flagProxyPort
	t.Cleanup(func() { *flagSMTPMXHost, *flagSMTPMXPort, *flagProxyPort = oldMX, oldPort, oldProxyPort })
	// A name that resolves to the listener, which is what the job passes.
	*flagSMTPMXHost, *flagSMTPMXPort, *flagProxyPort = "localhost", port, port
	_ = host

	_ = checkSMTPProxyProtocol() // the delivery cannot finish against this fake

	select {
	case header := <-got:
		fields := strings.Fields(header)
		if len(fields) < 6 {
			t.Fatalf("header %q has %d fields, want 6", header, len(fields))
		}
		if net.ParseIP(fields[2]) == nil {
			t.Errorf("source field %q is not an IP (header %q)", fields[2], header)
		}
		if net.ParseIP(fields[3]) == nil {
			t.Errorf("destination field %q is not an IP (header %q)", fields[3], header)
		}
	default:
		t.Fatal("the listener saw no PROXY header")
	}
}
