package backend

import (
	"net"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// Once bind returns, the port is accepting. Readiness is reported after
// bind, so early clients must not get connection refused.
func TestBindTCPHoldsThePortBeforeReturning(t *testing.T) {
	ln, err := bindTCP("test", &config.ServiceConfig{})
	if err != nil {
		t.Fatalf("bindTCP: %v", err)
	}
	defer ln.Close()

	// connectable immediately, even with nothing accepting yet
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("port not accepting right after bind: %v", err)
	}
	c.Close()
}

// A bind failure must be a returned error, not a log line in a goroutine.
func TestBindTCPFailsLoudlyOnAPortInUse(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer first.Close()

	port := first.Addr().(*net.TCPAddr).Port
	if _, err := bindTCP("test", &config.ServiceConfig{Port: port}); err == nil {
		t.Fatal("binding an occupied port must fail")
	}
}

func TestBindTCPErrorNamesProtocolAndAddress(t *testing.T) {
	// the error ends up in startup logs; both fields make it diagnosable
	_, err := bindTCP("imap", &config.ServiceConfig{Address: "256.256.256.256", Port: 99999})
	if err == nil {
		t.Fatal("expected an error for an invalid address")
	}
	msg := err.Error()
	for _, want := range []string{"imap", "256.256.256.256"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
}
