package backend

import (
	"context"
	"net"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	authclient "github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/sqlvars"
)

// lmtpOff is a userdb with a rule on %s refusing delivery.
type lmtpOff struct{}

func (lmtpOff) Lookup(u string) (*protocol.UserInfo, error) {
	return &protocol.UserInfo{Username: u, Home: "/m/" + u}, nil
}

func (l lmtpOff) LookupFor(u string, v sqlvars.Vars) (*protocol.UserInfo, error) {
	if v.Service == "lmtp" {
		return nil, nil
	}
	return l.Lookup(u)
}

func (lmtpOff) UsesRequest() bool { return true }

// The recipient lookup names lmtp, so a userdb rule refuses the delivery; the
// owner lookup names nothing and is answered (#2227).
func TestTheRecipientLookupNamesLMTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go protocol.NewMasterServer(lmtpOff{}).Serve(ctx, ln) //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() })            //nolint:errcheck
	addr := ln.Addr().String()
	dial := func() (*authclient.Client, error) { return authclient.Dial(addr, nil) }

	delivery := lazyUserdbLookup(addr, "lmtp", dial, &mailbox.Resolver{})
	if ui, err := delivery(context.Background(), "alice@d.test"); err != nil || ui != nil {
		t.Errorf("delivery lookup: %v, %v; want the recipient unknown", ui, err)
	}
	owner := ownerUserdbLookup(addr, nil, &mailbox.Resolver{})
	if ui, err := owner(context.Background(), "alice@d.test"); err != nil || ui == nil {
		t.Errorf("owner lookup: %v, %v; want alice", ui, err)
	}
}
