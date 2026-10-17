package jmaplogin

import (
	"context"
	"net"
	"net/http"
	"testing"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/pkg/sqlvars"
)

// ruled is a userdb with a rule on %s: alice is unknown to the protocol off.
type ruled struct{ off string }

func (ruled) Lookup(u string) (*protocol.UserInfo, error) {
	if u != "alice" {
		return nil, nil
	}
	return &protocol.UserInfo{Username: u}, nil
}

func (r ruled) LookupFor(u string, v sqlvars.Vars) (*protocol.UserInfo, error) {
	if v.Service == r.off {
		return nil, nil
	}
	return r.Lookup(u)
}

func (ruled) UsesRequest() bool { return true }

// alicePassword accepts alice/secret.
type alicePassword struct{}

func (alicePassword) Authenticate(req *protocol.Request) (protocol.Result, error) {
	if req.Username != "alice" || req.Password != "secret" {
		return protocol.ResultNext, nil
	}
	req.Fields.Set("user", "alice")
	return protocol.ResultOK, nil
}

// A rule on jmap refuses JMAP, Basic and Bearer alike, while IMAP logs in; a
// rule on imap the reverse (#2227).
func TestAProtocolRuleHoldsForJMAPBasicAndBearer(t *testing.T) {
	for _, off := range []string{"jmap", "imap"} {
		t.Run(off+" off", func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			chain := append([]protocol.Passdb{alicePassword{}}, oauthChain(t)...)
			go protocol.NewServer(chain, protocol.WithUserdb(ruled{off})).Serve(ctx, ln) //nolint:errcheck
			t.Cleanup(func() { cancel(); ln.Close() })                                   //nolint:errcheck
			cl, err := authclient.Dial(ln.Addr().String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cl.Close() }) //nolint:errcheck
			svc := NewAuthService(cl)
			base := startProxy(t, Options{Auth: svc, Tokens: svc})

			want := http.StatusOK
			if off == "jmap" {
				want = http.StatusUnauthorized
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/.well-known/jmap", nil)
			req.SetBasicAuth("alice", "secret")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close() //nolint:errcheck
			if resp.StatusCode != want {
				t.Errorf("Basic: status %d, want %d", resp.StatusCode, want)
			}
			if status, _ := bearerGet(t, base, "good-token"); status != want {
				t.Errorf("Bearer: status %d, want %d", status, want)
			}
			_, err = cl.Authenticate("alice", "secret", "imap", "", "", "")
			if imapOK := err == nil; imapOK != (off == "jmap") {
				t.Errorf("IMAP through the same service: err %v", err)
			}
		})
	}
}
