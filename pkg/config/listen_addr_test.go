package config

import "testing"

// An empty address is the role's host; a set one is the operator's, whatever
// the role (#2416).
func TestListenAddr(t *testing.T) {
	for _, tc := range []struct {
		address, role, want, from string
	}{
		{"", BackendHost, "127.0.0.1:10143", "role"},
		{"", PublicHost, ":10143", "role"},
		{"0.0.0.0", BackendHost, "0.0.0.0:10143", "address"},
		{"192.0.2.7", PublicHost, "192.0.2.7:10143", "address"},
		{"::1", BackendHost, "[::1]:10143", "address"},
	} {
		addr, from := (&ServiceConfig{Address: tc.address, Port: 10143}).ListenAddr(tc.role)
		if addr != tc.want || from != tc.from {
			t.Errorf("address %q, role %q: %q from %s, want %q from %s", tc.address, tc.role, addr, from, tc.want, tc.from)
		}
	}
	if addr, from := (*ServiceConfig)(nil).ListenAddr(PublicHost); addr != "" || from != "" {
		t.Errorf("nil service: %q from %q, want nothing", addr, from)
	}
}
