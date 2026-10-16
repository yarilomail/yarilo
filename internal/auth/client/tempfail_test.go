package client_test

import (
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	"github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// outage is a passdb whose backend is down.
type outage struct{}

func (outage) Authenticate(*protocol.Request) (protocol.Result, error) {
	return protocol.ResultTempFail, errors.New("backend down")
}

// The service's own temp-fail answer, not a stub's idea of it: a fake that
// spoke the wrong form hid that the client never read the real one (#2250).
func TestATempFailFromTheServiceIsReadAsOne(t *testing.T) {
	c := authtest.RelayToChain(t, outage{})
	if _, err := c.Authenticate("alice", "pass", "imap", "", "", ""); !errors.Is(err, client.ErrTempFail) {
		t.Fatalf("err = %v, want ErrTempFail", err)
	}
}
