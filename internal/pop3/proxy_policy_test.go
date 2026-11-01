package pop3

import (
	"net"
	"testing"

	"github.com/pires/go-proxyproto"
)

// A PROXY header is used only from a trusted network; from anywhere else, and
// from a peer that is not TCP, it is ignored and the connection's own address stays.
func TestProxyHeaderIsUsedOnlyFromATrustedNet(t *testing.T) {
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	for _, tc := range []struct {
		name     string
		nets     []*net.IPNet
		upstream net.Addr
		want     proxyproto.Policy
	}{
		{"no trusted nets", nil, &net.TCPAddr{IP: net.ParseIP("10.1.2.3")}, proxyproto.IGNORE},
		{"trusted peer", []*net.IPNet{trusted}, &net.TCPAddr{IP: net.ParseIP("10.1.2.3")}, proxyproto.USE},
		{"untrusted peer", []*net.IPNet{trusted}, &net.TCPAddr{IP: net.ParseIP("192.0.2.7")}, proxyproto.IGNORE},
		{"not TCP", []*net.IPNet{trusted}, &net.UnixAddr{Name: "/run/x.sock", Net: "unix"}, proxyproto.IGNORE},
	} {
		got, err := proxyPolicy(tc.nets)(proxyproto.ConnPolicyOptions{Upstream: tc.upstream})
		if err != nil || got != tc.want {
			t.Errorf("%s: policy %v, err %v; want %v", tc.name, got, err, tc.want)
		}
	}
}
