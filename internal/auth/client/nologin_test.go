package client_test

import (
	"context"
	"net"
	"testing"

	authclient "github.com/yarilomail/yarilo/v2/internal/auth/client"
	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
)

// flagged accepts alice/secret and sets nologin to the given value.
type flagged struct{ nologin string }

func (f flagged) Authenticate(req *protocol.Request) (protocol.Result, error) {
	if req.Username != "alice" || req.Password != "secret" {
		return protocol.ResultNext, nil
	}
	req.Fields.Set("nologin", f.nologin)
	return protocol.ResultOK, nil
}

// The service sends a passdb's nologin as key=value; the client reads it, and
// a false value stays a login (#2229).
func TestAPassdbNologinReachesTheClient(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"yes", true}, {"no", false}} {
		t.Run(tc.value, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			go protocol.NewServer([]protocol.Passdb{flagged{tc.value}}).Serve(ctx, ln) //nolint:errcheck
			t.Cleanup(func() { cancel(); ln.Close() })                                 //nolint:errcheck
			c, err := authclient.Dial(ln.Addr().String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() }) //nolint:errcheck
			res, err := c.Authenticate("alice", "secret", "imap", "192.0.2.1", "", "s1")
			if err != nil {
				t.Fatal(err)
			}
			if res.Nologin != tc.want {
				t.Errorf("nologin=%s read as Nologin %v, want %v", tc.value, res.Nologin, tc.want)
			}
		})
	}
}
