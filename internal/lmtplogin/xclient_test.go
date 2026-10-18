package lmtplogin

import (
	"bufio"
	"net"
	"strings"
	"testing"

	goSmtp "github.com/emersion/go-smtp"
)

func mustCIDRs(t *testing.T, ss ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", s, err)
		}
		out = append(out, n)
	}
	return out
}

// TestSessionXClient_TrustGate: a forwarded XCLIENT ADDR overrides peerIP only
// when the immutable socket peer is inside xclient.trusted_nets (#742).
func TestSessionXClient_TrustGate(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")
	cases := []struct {
		name       string
		socketIP   string
		attr       goSmtp.XClientAttrs
		wantPeerIP string
	}{
		{"trusted relay applies forward", "10.1.2.3", goSmtp.XClientAttrs{Addr: "203.0.113.9"}, "203.0.113.9"},
		{"untrusted relay ignored", "198.51.100.7", goSmtp.XClientAttrs{Addr: "203.0.113.9"}, "198.51.100.7"},
		{"empty addr no-op", "10.1.2.3", goSmtp.XClientAttrs{Addr: ""}, "10.1.2.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{
				opts:     testOpts(Options{XClientNets: trusted}),
				socketIP: tc.socketIP,
				peerIP:   tc.socketIP,
			}
			s.XClient(tc.attr)
			if s.peerIP != tc.wantPeerIP {
				t.Fatalf("peerIP = %q, want %q", s.peerIP, tc.wantPeerIP)
			}
		})
	}
}

func TestIPInNets(t *testing.T) {
	nets := mustCIDRs(t, "10.0.0.0/8", "127.0.0.1/32")
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"10.9.9.9", true}, {"127.0.0.1", true}, {"203.0.113.9", false}, {"bad", false},
	} {
		if got := ipInNets(tc.ip, nets); got != tc.want {
			t.Errorf("ipInNets(%q) = %v, want %v", tc.ip, got, tc.want)
		}
	}
	if ipInNets("10.0.0.1", nil) {
		t.Error("empty nets must trust nobody")
	}
}

// LHLO lists XCLIENT only to a relay in general.xclient.trusted_nets.
func TestLHLOAdvertisesXClientOnlyToTrustedRelays(t *testing.T) {
	cases := []struct {
		name string
		nets []*net.IPNet
		want bool
	}{
		{"a trusted relay", mustCIDRs(t, "127.0.0.0/8"), true},
		{"a relay outside trusted_nets", mustCIDRs(t, "10.0.0.0/8"), false},
		{"trusted_nets empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := New(testOpts(Options{Hostname: "lmtp.test", XClient: true, XClientNets: c.nets}))
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go s.srv.Serve(l) //nolint:errcheck
			defer s.srv.Close()
			cli, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			rd := bufio.NewReader(cli)
			rd.ReadString('\n') // greeting
			cli.Write([]byte("LHLO relay\r\n"))
			var caps []string
			for {
				line, err := rd.ReadString('\n')
				if err != nil {
					t.Fatalf("LHLO: %v after %v", err, caps)
				}
				caps = append(caps, strings.TrimSpace(line))
				if strings.HasPrefix(line, "250 ") {
					break
				}
			}
			if got := strings.Contains(strings.Join(caps, "\n"), "XCLIENT"); got != c.want {
				t.Errorf("XCLIENT listed %v, want %v: %v", got, c.want, caps)
			}
		})
	}
}
