package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/backendapi"
	"github.com/yarilomail/yarilo/v2/internal/warden"
)

// warden dump shows the connection a session holds, in both forms, and a
// kick reaches the channels it names; without a warden both say so.
func TestWardenVerbsAgainstTheBackend(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go warden.NewServer(10).Serve(ctx, ln) //nolint:errcheck
	addr := ln.Addr().String()
	c, err := warden.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if err := c.Connect("s1", liveUser, "192.0.2.7", "imap"); err != nil {
		t.Fatal(err)
	}

	startLiveBackendWith(t, func(o *backendapi.Options) { o.WardenAddr = addr })
	var dump struct {
		Counters []struct {
			Service string `json:"service"`
			UserIP  string `json:"user_ip"`
			Counter int    `json:"counter"`
		} `json:"counters"`
	}
	decode(t, mustYarctl(t, "warden", "dump", "--output", "json"), &dump)
	if len(dump.Counters) != 1 || dump.Counters[0].UserIP != liveUser+"@192.0.2.7" || dump.Counters[0].Counter != 1 {
		t.Errorf("warden dump: %+v, want the one imap connection", dump.Counters)
	}
	if out := mustYarctl(t, "warden", "dump"); !strings.Contains(out, liveUser+"@192.0.2.7") || !strings.Contains(out, "imap") {
		t.Errorf("warden dump as a table:\n%s", out)
	}

	var kick struct {
		EmittedTo []string `json:"emitted_to"`
	}
	decode(t, mustYarctl(t, "backend", "sessions", "kick", "s1", "--user", liveUser, "--protocols", "imap,pop3"), &kick)
	if strings.Join(kick.EmittedTo, ",") != "kick:imap,kick:pop3" {
		t.Errorf("kick emitted to %v, want kick:imap and kick:pop3", kick.EmittedTo)
	}
	if _, err := yarctl(t, "backend", "sessions", "kick"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("kick without a session = %v, want its usage", err)
	}
	if _, err := yarctl(t, "warden", "dump", "extra"); err == nil {
		t.Error("warden dump took a stray argument")
	}

	startLiveBackend(t)
	if _, err := yarctl(t, "warden", "dump"); err == nil || !strings.Contains(err.Error(), "warden_addr") {
		t.Errorf("dump without a warden = %v, want it named", err)
	}
	if _, err := yarctl(t, "backend", "sessions", "kick", "s1"); err == nil || !strings.Contains(err.Error(), "warden_addr") {
		t.Errorf("kick without a warden = %v, want it named", err)
	}
}
