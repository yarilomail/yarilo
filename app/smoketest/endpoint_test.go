package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"testing"
)

// A spelling nothing implements is refused: falling back to one of the three
// would report a surface the operator did not ask for.
func TestTheTLSModeIsOneOfThree(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want tlsMode
		bad  bool
	}{
		{in: "ssl", want: tlsSSL},
		{in: "STARTTLS", want: tlsSTARTTLS},
		{in: " none ", want: tlsNone},
		{in: "tls", bad: true},
		{in: "", bad: true},
	} {
		got, err := parseTLSMode("imap-tls", tc.in)
		switch {
		case tc.bad && err == nil:
			t.Errorf("%q was accepted as a mode", tc.in)
		case !tc.bad && err != nil:
			t.Errorf("%q was refused: %v", tc.in, err)
		case !tc.bad && got != tc.want:
			t.Errorf("%q read as %q, want %q", tc.in, got, tc.want)
		}
	}
}

// plainGreeter answers a greeting and one command, optionally upgrading.
func plainGreeter(t *testing.T, offerSTLS bool) (host, port string) {
	t.Helper()
	_, cert, _, _ := issueMTLSFixtures(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck
				r := bufio.NewReader(c)
				fmt.Fprintf(c, "+OK plain ready\r\n")
				line, rerr := r.ReadString('\n')
				if rerr != nil {
					return
				}
				if !strings.HasPrefix(strings.ToUpper(line), "STLS") || !offerSTLS {
					fmt.Fprintf(c, "-ERR no\r\n")
					return
				}
				fmt.Fprintf(c, "+OK begin TLS\r\n")
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				if herr := tc.Handshake(); herr != nil {
					return
				}
				fmt.Fprintf(tc, "+OK secure\r\n")
			}(c)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	return h, p
}

// none reaches a plain server, starttls upgrades it, and starttls against a
// server that refuses the command fails rather than continuing in the clear.
func TestTheEndpointDialsWhatTheModeNames(t *testing.T) {
	restore := func() func() {
		oi := *flagInsecure
		*flagInsecure = true
		return func() { *flagInsecure = oi }
	}()
	defer restore()

	t.Run("none stays plain", func(t *testing.T) {
		host, port := plainGreeter(t, false)
		ep := endpoint{name: "probe", host: host, port: port, mode: tlsNone}
		conn, r, err := ep.dial()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close() //nolint:errcheck
		line, err := r.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "+OK") {
			t.Errorf("read %q, %v; want the plain greeting", line, err)
		}
	})

	t.Run("starttls upgrades", func(t *testing.T) {
		host, port := plainGreeter(t, true)
		ep := endpoint{name: "probe", host: host, port: port, mode: tlsSTARTTLS,
			upgrade: lineUpgrade("STLS", "+OK")}
		conn, r, err := ep.dial()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close() //nolint:errcheck
		if _, ok := conn.(*tls.Conn); !ok {
			t.Fatalf("the connection is %T, not a TLS one", conn)
		}
		line, err := r.ReadString('\n')
		if err != nil || !strings.Contains(line, "secure") {
			t.Errorf("read %q, %v; want the line the server sent inside TLS", line, err)
		}
	})

	t.Run("starttls refused is an error, not a plain session", func(t *testing.T) {
		host, port := plainGreeter(t, false)
		ep := endpoint{name: "probe", host: host, port: port, mode: tlsSTARTTLS,
			upgrade: lineUpgrade("STLS", "+OK")}
		conn, _, err := ep.dial()
		if err == nil {
			conn.Close() //nolint:errcheck
			t.Fatal("a server that refused the upgrade was accepted")
		}
		if !strings.Contains(err.Error(), "STLS refused") {
			t.Errorf("said %q, want it to name the refusal", err)
		}
	})

	t.Run("starttls with no upgrade for the protocol", func(t *testing.T) {
		host, port := plainGreeter(t, true)
		ep := endpoint{name: "probe", host: host, port: port, mode: tlsSTARTTLS}
		if _, _, err := ep.dial(); err == nil {
			t.Fatal("starttls was accepted for a check that cannot perform it")
		}
	})
}
