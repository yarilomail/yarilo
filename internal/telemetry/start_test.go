package telemetry

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

// A telemetry port that is taken stops the start: Start answers the bind
// before it returns, rather than leaving a process without /healthz and
// nothing but a log line to say so (#2419).
func TestStartReturnsABindFailure(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = NewWithOptions(Options{Addr: taken.Addr().String()}).Start(ctx)
	if err == nil || !strings.Contains(err.Error(), taken.Addr().String()) {
		t.Fatalf("Start on a taken port: %v; want a bind failure naming the address", err)
	}
}

func TestStartServesOnceItReturns(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := NewWithOptions(Options{Addr: addr}).Start(ctx); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("/healthz right after Start: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
}
