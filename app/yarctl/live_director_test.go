package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/director"
)

// startLiveDirector is a real director admin API with yarctl pointed at it.
func startLiveDirector(t *testing.T) {
	t.Helper()
	s := director.NewWithOptions(director.Options{
		UserExpire: 900 * time.Second, PingInterval: 30 * time.Second, PingTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, AntiEntropyInterval: 3 * time.Second, SeedPollInterval: 2 * time.Second,
		SeedPollIdleInterval: 2 * time.Second, BackendExpire: 30 * time.Second, UnreachableReporters: 2,
		UnreachableWindow: 5 * time.Second, UserKickDelay: 2 * time.Second, MaxParallelKicks: 100,
		MaxParallelMoves: 5, FlushProgramTimeout: 10 * time.Second, UserKillTimeout: 15 * time.Second,
		UserKillConfirmGrace: time.Second,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.StartAPI(ctx, ln, "director-token", nil); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	oldURL, oldToken, oldFormat := apiURL, apiToken, outputFormat
	apiURL, apiToken, outputFormat = "http://"+ln.Addr().String(), "director-token", "json"
	t.Cleanup(func() { apiURL, apiToken, outputFormat = oldURL, oldToken, oldFormat })
}

type liveBackendRow struct {
	IP     string `json:"ip"`
	Up     bool   `json:"up"`
	Vhosts int    `json:"vhosts"`
}

func directorBackends(t *testing.T) map[string]liveBackendRow {
	t.Helper()
	var resp struct {
		Backends []liveBackendRow `json:"backends"`
	}
	decode(t, mustYarctl(t, "director", "backends", "list"), &resp)
	out := map[string]liveBackendRow{}
	for _, b := range resp.Backends {
		out[b.IP] = b
	}
	return out
}

// A backend added without --vhosts takes the director's default weight; a
// named weight, 0 included, is the one it gets.
func TestBackendsAddKeepsTheWeightNamed(t *testing.T) {
	startLiveDirector(t)
	mustYarctl(t, "director", "backends", "add", "10.0.0.7", "--port", "143", "--tag", "a")
	mustYarctl(t, "director", "backends", "add", "10.0.0.8", "--port", "143", "--tag", "a", "--vhosts", "40")
	mustYarctl(t, "director", "backends", "add", "10.0.0.9", "--port", "143", "--tag", "a", "--vhosts", "0")
	got := directorBackends(t)
	for ip, want := range map[string]int{"10.0.0.7": 100, "10.0.0.8": 40, "10.0.0.9": 0} {
		if got[ip].Vhosts != want {
			t.Errorf("%s has weight %d, want %d", ip, got[ip].Vhosts, want)
		}
	}
}

// The backend and user verbs change the director's state as the next read shows.
func TestDirectorVerbsAgainstTheDirector(t *testing.T) {
	startLiveDirector(t)
	mustYarctl(t, "director", "backends", "add", "10.0.0.7", "--port", "143", "--tag", "a")
	mustYarctl(t, "director", "backends", "add", "10.0.0.8", "--port", "143", "--tag", "a")

	mustYarctl(t, "director", "backends", "update", "10.0.0.8", "--vhosts", "50")
	mustYarctl(t, "director", "backends", "down", "10.0.0.8")
	if b := directorBackends(t)["10.0.0.8"]; b.Vhosts != 50 || b.Up {
		t.Errorf("after update and down: %+v", b)
	}
	mustYarctl(t, "director", "backends", "up", "10.0.0.8")
	if b := directorBackends(t)["10.0.0.8"]; !b.Up {
		t.Errorf("after up: %+v", b)
	}

	mustYarctl(t, "director", "users", "move", "alice@example.com", "--backend", "10.0.0.8:143")
	var m struct {
		Backend string `json:"backend"`
		Pinned  bool   `json:"pinned"`
	}
	decode(t, mustYarctl(t, "director", "map", "--user", "alice@example.com"), &m)
	if m.Backend != "10.0.0.8" || !m.Pinned {
		t.Errorf("map after the move: %+v", m)
	}
	mustYarctl(t, "director", "users", "kick", "alice@example.com")

	var dump struct {
		Backends []liveBackendRow `json:"backends"`
	}
	decode(t, mustYarctl(t, "director", "dump"), &dump)
	if len(dump.Backends) != 2 {
		t.Errorf("dump holds %d backends, want 2", len(dump.Backends))
	}

	mustYarctl(t, "director", "backends", "flush", "10.0.0.7")
	mustYarctl(t, "director", "backends", "remove", "10.0.0.7")
	if _, ok := directorBackends(t)["10.0.0.7"]; ok {
		t.Error("a removed backend is still listed")
	}
	if _, err := yarctl(t, "director", "domains", "move", "example.com", "10.0.0.8"); err == nil {
		t.Error("a domain move without the domain policy succeeded")
	}
}

// The human forms print tables an operator reads.
func TestDirectorHumanOutput(t *testing.T) {
	startLiveDirector(t)
	mustYarctl(t, "director", "backends", "add", "10.0.0.7", "--port", "143", "--tag", "a")
	outputFormat = "human"
	list := mustYarctl(t, "director", "backends", "list")
	if !strings.Contains(list, "BACKEND") || !strings.Contains(list, "10.0.0.7:143") || !strings.Contains(list, "up") {
		t.Errorf("backends list:\n%s", list)
	}
	if out := mustYarctl(t, "director", "status"); !strings.Contains(out, "10.0.0.7:143") {
		t.Errorf("status:\n%s", out)
	}
	if out := mustYarctl(t, "director", "ring", "status"); !strings.HasPrefix(out, "ring status: 0 directors") {
		t.Errorf("ring status:\n%s", out)
	}
	if out := mustYarctl(t, "director", "backends", "down", "10.0.0.7"); out != "ok" {
		t.Errorf("down printed %q", out)
	}
	apiToken = "wrong"
	if _, err := yarctl(t, "director", "status"); err == nil {
		t.Error("a wrong token was accepted")
	}
}
