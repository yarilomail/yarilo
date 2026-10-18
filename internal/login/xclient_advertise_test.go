package login

import (
	"bufio"
	"net"
	"strings"
	"testing"
)

// Submission's EHLO lists XCLIENT only to a peer in general.xclient.trusted_nets.
func TestSubmissionAdvertisesXClientOnlyToTrustedPeers(t *testing.T) {
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	_, other, _ := net.ParseCIDR("10.0.0.0/8")
	cases := []struct {
		name string
		opts Options
		want bool
	}{
		{"a trusted peer", Options{XClient: true, XClientNets: []*net.IPNet{loopback}}, true},
		{"a peer outside trusted_nets", Options{XClient: true, XClientNets: []*net.IPNet{other}}, false},
		{"trusted_nets empty", Options{XClient: true}, false},
		{"XCLIENT off", Options{XClientNets: []*net.IPNet{loopback}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				srv, err := l.Accept()
				if err != nil {
					return
				}
				defer srv.Close()
				_, _, _, _ = extractSubmissionPreamble(srv, bufio.NewReader(srv), nil, testOpts(c.opts), relayContext{})
			}()
			cli, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			rd := bufio.NewReader(cli)
			rd.ReadString('\n') // greeting
			cli.Write([]byte("EHLO relay\r\n"))
			var caps []string
			for {
				line, err := rd.ReadString('\n')
				if err != nil {
					t.Fatalf("EHLO: %v after %v", err, caps)
				}
				caps = append(caps, strings.TrimSpace(line))
				if strings.HasPrefix(line, "250 ") {
					break
				}
			}
			cli.Close()
			<-done
			got := strings.Contains(strings.Join(caps, "\n"), "XCLIENT")
			if got != c.want {
				t.Errorf("XCLIENT listed %v, want %v: %v", got, c.want, caps)
			}
		})
	}
}
