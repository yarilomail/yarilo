package login

import (
	"bufio"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	goSmtp "github.com/emersion/go-smtp"
)

// ehloLines runs the submission preamble with opts and returns the EHLO
// reply's keywords, one per line, without the "250-"/"250 " prefix.
func ehloLines(t *testing.T, opts Options) []string {
	t.Helper()
	srv, cli := pipePair(t)
	go extractSubmissionPreamble(srv, bufio.NewReader(srv), nil, opts, relayContext{}) //nolint:errcheck
	cli.SetDeadline(time.Now().Add(5 * time.Second))                                   //nolint:errcheck
	rd := bufio.NewReader(cli)
	rd.ReadString('\n') //nolint:errcheck // greeting
	fmt.Fprintf(cli, "EHLO client\r\n")
	var out []string
	for _, l := range readUntil(t, rd, "250 ") {
		out = append(out, l[4:])
	}
	return out[1:] // the first line is the greeting
}

// The client keeps the login layer's EHLO for the whole session, so the
// extensions yarilo-submission serves must be listed here, with the limits
// read from the same protocol.submission settings, or the client never uses
// them: it cannot learn the size limit before sending the whole message.
func TestSubmissionEHLOListsWhatTheBackendServes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int64
		rcpt    int
		want    []string
		without []string
	}{
		{"limits set", 41943040, 50,
			[]string{"PIPELINING", "8BITMIME", "ENHANCEDSTATUSCODES", "CHUNKING", "SIZE 41943040", "LIMITS RCPTMAX=50"}, nil},
		{"another size, no recipient limit", 10485760, 0,
			[]string{"SIZE 10485760"}, []string{"SIZE 41943040", "LIMITS"}},
		{"no size limit", 0, 0,
			[]string{"SIZE"}, []string{"LIMITS"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ehloLines(t, Options{Protocol: ProtocolSubmission,
				SubmissionMaxMessageBytes: tc.size, SubmissionMaxRecipients: tc.rcpt})
			for _, w := range tc.want {
				if !slices.Contains(got, w) {
					t.Errorf("EHLO lacks %q: %q", w, got)
				}
			}
			for _, w := range tc.without {
				for _, g := range got {
					if g == w || strings.HasPrefix(g, w+" ") {
						t.Errorf("EHLO has %q: %q", g, got)
					}
				}
			}
		})
	}
}

// backendEHLO is what go-smtp, the server inside yarilo-submission, lists
// with the same limits, less the lines that depend on the connection.
func backendEHLO(t *testing.T, size int64, rcpt int) []string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := goSmtp.NewServer(nopBackend{})
	srv.MaxMessageBytes = size
	srv.MaxRecipients = rcpt
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	rd := bufio.NewReader(c)
	rd.ReadString('\n') //nolint:errcheck // greeting
	fmt.Fprintf(c, "EHLO client\r\n")
	var out []string
	for _, l := range readUntil(t, rd, "250 ")[1:] {
		if k := l[4:]; !strings.HasPrefix(k, "AUTH") && k != "STARTTLS" {
			out = append(out, k)
		}
	}
	return out
}

// The proxy's list is the backend's, line for line, for each shape of limits.
func TestSubmissionEHLOMatchesTheBackendLineForLine(t *testing.T) {
	for _, tc := range []struct {
		size int64
		rcpt int
	}{{41943040, 50}, {10485760, 0}, {0, 0}} {
		t.Run(fmt.Sprintf("size=%d,rcpt=%d", tc.size, tc.rcpt), func(t *testing.T) {
			var proxy []string
			for _, k := range ehloLines(t, Options{Protocol: ProtocolSubmission,
				SubmissionMaxMessageBytes: tc.size, SubmissionMaxRecipients: tc.rcpt}) {
				if !strings.HasPrefix(k, "AUTH") && k != "STARTTLS" && !strings.HasPrefix(k, "XCLIENT") {
					proxy = append(proxy, k)
				}
			}
			backend := backendEHLO(t, tc.size, tc.rcpt)
			slices.Sort(proxy)
			slices.Sort(backend)
			if !slices.Equal(proxy, backend) {
				t.Errorf("proxy EHLO %q, backend EHLO %q", proxy, backend)
			}
		})
	}
}
