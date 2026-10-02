package main

import (
	"context"
	"net"
	"testing"

	"github.com/yarilomail/yarilo/internal/director"
	"github.com/yarilomail/yarilo/pkg/config"
)

// The admin API is bound before startAPI returns: a taken port or a refused
// config is an error the caller exits on, not a log line after ready.
func TestTheDirectorAPIIsBoundBeforeReady(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { taken.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	noFail := func(err error) { t.Errorf("serve failed: %v", err) }

	if err := startAPI(ctx, director.New(), config.DirectorAPIConfig{Listen: taken.Addr().String(), Token: "t"}, noFail); err == nil {
		t.Error("a taken port started the API")
	}
	if err := startAPI(ctx, director.New(), config.DirectorAPIConfig{Listen: "127.0.0.1:0"}, noFail); err == nil {
		t.Error("an empty token started the API")
	}

	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	free.Close()
	if err := startAPI(ctx, director.New(), config.DirectorAPIConfig{Listen: addr, Token: "t"}, noFail); err != nil {
		t.Fatalf("a free port: %v", err)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("the API is not listening when startAPI returns: %v", err)
	}
	c.Close()
}
