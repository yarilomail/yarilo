package jmaplogin

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"
)

// addrAuth accepts u1 and keeps the client address each login was asked from.
type addrAuth struct {
	mu  sync.Mutex
	ips []string
}

func (a *addrAuth) Authenticate(username, password, _, remoteIP, _, _ string) (Login, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ips = append(a.ips, remoteIP)
	if username != "u1" || password != "pw" {
		return Login{}, errors.New("bad credentials")
	}
	return Login{User: username}, nil
}

func (a *addrAuth) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ips) == 0 {
		return ""
	}
	return a.ips[len(a.ips)-1]
}

func cidrs(t *testing.T, ss ...string) []*net.IPNet {
	t.Helper()
	var nets []*net.IPNet
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		nets = append(nets, n)
	}
	return nets
}

// proxiedClient dials the proxy and, when source is set, opens the connection
// with a PROXY v2 header naming it, as a load balancer in front would.
func proxiedClient(source string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 1,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := new(net.Dialer).DialContext(ctx, network, addr)
				if err != nil || source == "" {
					return c, err
				}
				h := &proxyproto.Header{
					Version:           2,
					Command:           proxyproto.PROXY,
					TransportProtocol: proxyproto.TCPv4,
					SourceAddr:        &net.TCPAddr{IP: net.ParseIP(source), Port: 40000},
					DestinationAddr:   c.RemoteAddr(),
				}
				if _, err := h.WriteTo(c); err != nil {
					c.Close() //nolint:errcheck
					return nil, err
				}
				return c, nil
			},
		},
	}
}

func getWith(t *testing.T, c *http.Client, base string, header http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/.well-known/jmap", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header.Clone()
	req.Header.Set("Authorization", basic("u1", "pw"))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// The client address is the header's only when the peer, after the PROXY
// header, is a trusted proxy and the key names the header; the warden, the
// per-IP limits it applies and the passdb all see that one address (#2512).
func TestClientIPHeader(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    string
		nets   []string
		proxy  string // PROXY v2 source; empty sends no PROXY header
		header http.Header
		want   string
	}{
		{name: "trusted peer", key: "CF-Connecting-IP", nets: []string{"127.0.0.0/8"},
			header: http.Header{"Cf-Connecting-Ip": {"203.0.113.9"}}, want: "203.0.113.9"},
		{name: "untrusted peer", key: "CF-Connecting-IP", nets: []string{"10.0.0.0/8"},
			header: http.Header{"Cf-Connecting-Ip": {"203.0.113.9"}}, want: "127.0.0.1"},
		{name: "empty key", nets: []string{"127.0.0.0/8"},
			header: http.Header{"Cf-Connecting-Ip": {"203.0.113.9"}}, want: "127.0.0.1"},
		{name: "several hops", key: "X-Forwarded-For", nets: []string{"127.0.0.0/8"},
			header: http.Header{"X-Forwarded-For": {"198.51.100.1, 192.0.2.44", "198.51.100.2, 203.0.113.9"}}, want: "203.0.113.9"},
		{name: "not an address", key: "CF-Connecting-IP", nets: []string{"127.0.0.0/8"},
			header: http.Header{"Cf-Connecting-Ip": {"unknown"}}, want: "127.0.0.1"},
		{name: "with a port", key: "CF-Connecting-IP", nets: []string{"127.0.0.0/8"},
			header: http.Header{"Cf-Connecting-Ip": {"203.0.113.9:443"}}, want: "127.0.0.1"},
		{name: "in brackets", key: "CF-Connecting-IP", nets: []string{"127.0.0.0/8"},
			header: http.Header{"Cf-Connecting-Ip": {"[2001:db8::1]"}}, want: "127.0.0.1"},
		{name: "with a zone", key: "CF-Connecting-IP", nets: []string{"127.0.0.0/8"},
			header: http.Header{"Cf-Connecting-Ip": {"fe80::1%eth0"}}, want: "127.0.0.1"},
		{name: "IPv6", key: "CF-Connecting-IP", nets: []string{"127.0.0.0/8"},
			header: http.Header{"Cf-Connecting-Ip": {"2001:db8::1"}}, want: "2001:db8::1"},
		{name: "trusted after the PROXY header", key: "CF-Connecting-IP", nets: []string{"127.0.0.0/8", "10.0.0.0/8"},
			proxy: "10.1.1.1", header: http.Header{"Cf-Connecting-Ip": {"203.0.113.9"}}, want: "203.0.113.9"},
		{name: "untrusted after the PROXY header", key: "CF-Connecting-IP", nets: []string{"127.0.0.0/8"},
			proxy: "192.0.2.7", header: http.Header{"Cf-Connecting-Ip": {"203.0.113.9"}}, want: "192.0.2.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wd, auth := &fakeWarden{}, &addrAuth{}
			base := startProxy(t, Options{Warden: wd, Auth: auth, ClientIPHeader: tc.key,
				ProxyProtocol: tc.proxy != "", HAProxyTrustedNets: cidrs(t, tc.nets...), HAProxyTimeout: time.Second})
			getWith(t, proxiedClient(tc.proxy), base, tc.header)
			calls := wd.snapshot()
			if len(calls) != 1 || calls[0].ip != tc.want {
				t.Errorf("warden was given %+v, want one connect from %s", calls, tc.want)
			}
			if got := auth.last(); got != tc.want {
				t.Errorf("the passdb was asked from %q, want %q", got, tc.want)
			}
		})
	}
}

// A proxy's pooled connection carries different clients in turn: each is
// accounted under its own address, never the first one's.
func TestClientIPHeaderReaccountsWhenTheAddressChanges(t *testing.T) {
	wd := &fakeWarden{}
	base := startProxy(t, Options{Warden: wd, Auth: &addrAuth{}, ClientIPHeader: "CF-Connecting-IP",
		HAProxyTrustedNets: cidrs(t, "127.0.0.0/8")})
	c := proxiedClient("")
	for _, ip := range []string{"203.0.113.9", "203.0.113.10"} {
		getWith(t, c, base, http.Header{"Cf-Connecting-Ip": {ip}})
	}
	want := []wardenCall{{op: "connect", ip: "203.0.113.9"}, {op: "disconnect", ip: "203.0.113.9"}, {op: "connect", ip: "203.0.113.10"}}
	calls := wd.snapshot()
	if len(calls) != len(want) {
		t.Fatalf("warden calls %+v, want %+v", calls, want)
	}
	for i, w := range want {
		if calls[i].op != w.op || calls[i].ip != w.ip {
			t.Errorf("call %d = %s from %s, want %s from %s", i, calls[i].op, calls[i].ip, w.op, w.ip)
		}
	}
}

// A trusted proxy sending garbage is logged once per connection, cut short,
// and the peer stays the client address.
func TestClientIPHeaderGarbageIsLoggedOncePerConnection(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	wd := &fakeWarden{}
	base := startProxy(t, Options{Warden: wd, Auth: &addrAuth{}, ClientIPHeader: "CF-Connecting-IP",
		HAProxyTrustedNets: cidrs(t, "127.0.0.0/8")})
	c := proxiedClient("")
	garbage := strings.Repeat("x", 200)
	for range 2 {
		getWith(t, c, base, http.Header{"Cf-Connecting-Ip": {garbage}})
	}
	out := buf.String()
	if n := strings.Count(out, "client address header is not an address"); n != 1 {
		t.Errorf("logged %d times over one connection, want once:\n%s", n, out)
	}
	if strings.Contains(out, strings.Repeat("x", 65)) || !strings.Contains(out, strings.Repeat("x", 64)) {
		t.Errorf("the value is not cut to 64 bytes:\n%s", out)
	}
	if calls := wd.snapshot(); len(calls) != 1 || calls[0].ip != "127.0.0.1" {
		t.Errorf("warden was given %+v, want the peer", calls)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
