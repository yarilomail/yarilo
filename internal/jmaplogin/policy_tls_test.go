package jmaplogin

import (
	"context"
	"net"
	"testing"

	authclient "github.com/yarilomail/yarilo/v2/internal/auth/client"
	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
)

type tlsPolicy struct{ tls chan bool }

func (p tlsPolicy) CheckBefore(_ context.Context, r protocol.PolicyRequest) (protocol.PolicyDecision, error) {
	p.tls <- r.TLS
	return protocol.PolicyDecision{Continue: true}, nil
}

func (tlsPolicy) CheckAfter(context.Context, protocol.PolicyRequest, bool, bool) (protocol.PolicyDecision, error) {
	return protocol.PolicyDecision{Continue: true}, nil
}

func (tlsPolicy) ReportAfter(context.Context, protocol.PolicyRequest, bool, bool) {}

// JMAP is served over HTTPS only, so the policy hears every JMAP login as TLS.
func TestAJMAPLoginIsTLSToThePolicy(t *testing.T) {
	p := tlsPolicy{tls: make(chan bool, 1)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := protocol.NewServer([]protocol.Passdb{alicePassword{}}, protocol.WithPolicy(p, protocol.PolicyMode{CheckBefore: true}))
	go srv.Serve(ctx, ln)                      //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() }) //nolint:errcheck
	cl, err := authclient.Dial(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() }) //nolint:errcheck
	_, _ = NewAuthService(cl).Authenticate("alice", "secret", "jmap", "192.0.2.1", "", "s1")
	if !<-p.tls {
		t.Error("the policy heard a JMAP login as not TLS")
	}
}
