package backendapi

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/director"
)

// The director router answers the address the director's own lookup gives,
// without its port: the backend-api port is this deployment's, not the
// session's. No director address is no router at all.
func TestDirectorRouterAsksTheDirector(t *testing.T) {
	if NewDirectorRouter("", "", nil) != nil {
		t.Fatal("a router without a director address")
	}
	d := director.NewWithOptions(director.Options{
		UserExpire: time.Minute, PingInterval: 30 * time.Second, PingTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, AntiEntropyInterval: 3 * time.Second, SeedPollInterval: 2 * time.Second,
		SeedPollIdleInterval: 2 * time.Second, BackendExpire: 30 * time.Second, UnreachableReporters: 2,
		UnreachableWindow: 5 * time.Second, UserKickDelay: 2 * time.Second, MaxParallelKicks: 10,
		MaxParallelMoves: 5, FlushProgramTimeout: 10 * time.Second, UserKillTimeout: 15 * time.Second,
		UserKillConfirmGrace: time.Second, DomainExpire: time.Minute, DomainRebalanceInterval: time.Minute,
	})
	d.AddBackend("10.0.0.9", 143, "", 100)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Serve(ctx, ln) //nolint:errcheck

	r := NewDirectorRouter(ln.Addr().String(), "", nil)
	for _, user := range []string{"alice@example.com", "bob@example.com"} {
		got, err := r.Lookup(user)
		if err != nil || got != "10.0.0.9" {
			t.Errorf("Lookup(%s) = %q, %v; want the one backend's address", user, got, err)
		}
	}

	_ = ln.Close()
	cancel()
	if _, err := NewDirectorRouter("127.0.0.1:1", "", nil).Lookup("alice@example.com"); err == nil {
		t.Error("an unreachable director answered a lookup")
	}
}
