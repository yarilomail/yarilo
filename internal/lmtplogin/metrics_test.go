package lmtplogin

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func phaseCount(t *testing.T, phase string) uint64 {
	t.Helper()
	var m dto.Metric
	if err := rcptPhaseSeconds.WithLabelValues(phase).(interface{ Write(*dto.Metric) error }).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// One RCPT records each phase it walks once, and none it skips (#2149).
func TestRcptRecordsEachPhase(t *testing.T) {
	phases := []string{phaseUserdb, phaseDirectorDial, phaseDirectorLookup, phaseWardenDial, phaseWardenLookup, phaseWardenConnect, phaseToken}
	before := map[string]uint64{}
	for _, p := range phases {
		before[p] = phaseCount(t, p)
	}

	var captured string
	directorAddr := startStubDirector(t, &captured)
	s := &session{opts: Options{DirectorAddr: directorAddr}}
	_, _ = s.directorLookup("user@example.com", "")

	stub, backendAddr := newStubBackend(t)
	proxyAddr := startLMTPLogin(t, Options{
		Hostname:         "test.local",
		BackendAddr:      backendAddr,
		AuthMasterAddr:   startTestAuth(t),
		WardenAddr:       startTestWarden(t),
		ConcurrencyLimit: 5,
	})
	mta := dialMTA(t, proxyAddr)
	mta.lmtpHandshake(t)
	mta.mailFrom(t, "sender@example.com")
	mta.rcpt(t, "alice@example.com")
	mta.data(t, "Subject: Test\r\n\r\nHello")
	mta.readDataStatuses(t, 1)
	mta.quit(t)
	stub.get(t, 3*time.Second)

	for _, p := range phases {
		if got := phaseCount(t, p) - before[p]; got != 1 {
			t.Errorf("phase %s observed %d times, want 1", p, got)
		}
	}
}
