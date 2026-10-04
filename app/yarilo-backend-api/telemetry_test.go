package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/config"
)

// The admin API answers /metrics from its own process, and the counters an
// admin command moves are in that page (#1999).
func TestTheAdminAPIServesItsOwnMetrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	t.Setenv("TELEMETRY_LISTEN", addr)
	go runTelemetry(config.TelemetryConfig{})

	var body string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, rerr := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://"+addr+"/metrics", nil)
		if rerr != nil {
			t.Fatal(rerr)
		}
		resp, gerr := http.DefaultClient.Do(req)
		if gerr != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		raw, berr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if berr != nil {
			t.Fatal(berr)
		}
		body = string(raw)
		break
	}
	if body == "" {
		t.Fatal("the telemetry server never answered, so the admin API's counters are unreadable")
	}
	// A counter only this binary's registry carries: reading it off a
	// neighbour's page is what the port exists to stop.
	if !strings.Contains(body, "fileindex_guid_store_rebuilt_total") {
		t.Errorf("the page carries no fileindex_guid_store_rebuilt_total, so a rebuild reads as nothing")
	}
}

// And main starts it: a telemetry server nobody launches answers as little as
// none at all, which is the state this binary shipped in (#1999).
func TestMainStartsTheTelemetryServer(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "go runTelemetry(cfg.Telemetry)") {
		t.Error("main does not start the telemetry server, so the admin API's counters are unreachable")
	}
}
