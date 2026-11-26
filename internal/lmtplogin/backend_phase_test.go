package lmtplogin

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	goSmtp "github.com/emersion/go-smtp"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/v2/internal/loginproto"
)

func backendPhaseCount(t *testing.T, phase string) uint64 {
	t.Helper()
	var m dto.Metric
	if err := backendPhaseSeconds.WithLabelValues(phase).(interface{ Write(*dto.Metric) error }).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

func handshakes(t *testing.T, resumed string) float64 {
	t.Helper()
	var m dto.Metric
	if err := backendTLSHandshakes.WithLabelValues(resumed).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

var allBackendPhases = []string{backendPhaseDial, backendPhaseSetup, backendPhaseData}

func phaseCounts(t *testing.T) map[string]uint64 {
	out := map[string]uint64{}
	for _, p := range allBackendPhases {
		out[p] = backendPhaseCount(t, p)
	}
	return out
}

func deliverOnce(addr string, tlsCfg *tls.Config) error {
	return fanOutOne(addr, "test.local", "a@example.com", "u@example.com", goSmtp.RcptOptions{},
		[]byte("Subject: x\r\n\r\nbody\r\n"), loginproto.Preamble{Addr: "127.0.0.1", User: "u@example.com"},
		5*time.Second, tlsCfg)
}

// One delivery observes each of the three backend phases once.
func TestADeliveryObservesEachBackendPhaseOnce(t *testing.T) {
	_, addr := newStubBackend(t)
	before := phaseCounts(t)
	if err := deliverOnce(addr, nil); err != nil {
		t.Fatal(err)
	}
	after := phaseCounts(t)
	for _, p := range allBackendPhases {
		if d := after[p] - before[p]; d != 1 {
			t.Errorf("phase %s observed %d times for one delivery, want 1", p, d)
		}
	}
}

// A phase an error ended is not observed, nor any after it: a backend that
// refuses the RCPT completes the dial and nothing more.
func TestAPhaseAnErrorEndedIsNotObserved(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		rd := bufio.NewReader(c)
		_, _ = rd.ReadString('\n') // preamble
		fmt.Fprintf(c, "220 refuse\r\n")
		for _, reply := range []string{"250 hi", "250 OK", "550 5.1.1 no such user"} {
			if _, rerr := rd.ReadString('\n'); rerr != nil {
				return
			}
			fmt.Fprintf(c, "%s\r\n", reply)
		}
		_, _ = rd.ReadString('\n')
	}()
	before := phaseCounts(t)
	if err := deliverOnce(ln.Addr().String(), nil); err == nil || !strings.Contains(err.Error(), "no such user") {
		t.Fatalf("delivery = %v, want the RCPT refusal", err)
	}
	after := phaseCounts(t)
	for p, want := range map[string]uint64{backendPhaseDial: 1, backendPhaseSetup: 0, backendPhaseData: 0} {
		if d := after[p] - before[p]; d != want {
			t.Errorf("phase %s observed %d times, want %d", p, d, want)
		}
	}
}

// The handshake counter tells the first connection from one that resumed its
// TLS session.
func TestTheTLSHandshakeCounterTellsAResumedSession(t *testing.T) {
	serverCfg, clientCfg := selfSignedTLS(t)
	clientCfg.ClientSessionCache = tls.NewLRUClientSessionCache(8)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	sb := &stubBackend{}
	go sb.serve(ln)

	full, resumed := handshakes(t, "false"), handshakes(t, "true")
	if err := deliverOnce(ln.Addr().String(), clientCfg); err != nil {
		t.Fatal(err)
	}
	if err := deliverOnce(ln.Addr().String(), clientCfg); err != nil {
		t.Fatal(err)
	}
	if d := handshakes(t, "false") - full; d != 1 {
		t.Errorf("full handshakes +%v, want 1 (the first connection)", d)
	}
	if d := handshakes(t, "true") - resumed; d != 1 {
		t.Errorf("resumed handshakes +%v, want 1 (the second connection)", d)
	}
}
