package backend

import (
	"net"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// Every session listener here binds loopback when its address is empty, the
// single binary's included: logins reach it only through a login proxy's
// preamble (#2416). A set address is the operator's.
func TestASessionListenerTakesTheBackendHost(t *testing.T) {
	if sessionHost != config.BackendHost {
		t.Fatalf("sessionHost = %q, want the backend host %q", sessionHost, config.BackendHost)
	}
	for _, tc := range []struct {
		address  string
		loopback bool
	}{
		{"", true},
		{"0.0.0.0", false},
	} {
		ln, err := bindTCP("imap", &config.ServiceConfig{Enabled: true, Address: tc.address})
		if err != nil {
			t.Fatal(err)
		}
		ip := ln.Addr().(*net.TCPAddr).IP
		ln.Close()
		if ip.IsLoopback() != tc.loopback {
			t.Errorf("address %q bound %s, loopback = %v, want %v", tc.address, ip, ip.IsLoopback(), tc.loopback)
		}
	}
}
