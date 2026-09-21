package mailboxmetrics

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func refusals(t *testing.T, driver, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(writeFailed.WithLabelValues(driver, reason))
}

// A full volume refuses the body, not the journal, so the class has to be named
// here or the commonest failure reaches the client as a fault.
func TestClassifyWriteNamesTheRefusal(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		noSpace bool
		reason  string
	}{
		{name: "a full volume", err: syscall.ENOSPC, noSpace: true, reason: "no-space"},
		{name: "an exhausted quota", err: syscall.EDQUOT, noSpace: true, reason: "no-space"},
		{name: "wrapped, as a driver would", err: fmt.Errorf("write: %w", syscall.ENOSPC), noSpace: true, reason: "no-space"},
		{name: "a failing disk", err: syscall.EIO, noSpace: false, reason: "other"},
		{name: "a refused path", err: syscall.EACCES, noSpace: false, reason: "other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			was := refusals(t, "testdriver", tc.reason)
			got := ClassifyWrite("testdriver", "Drafts", tc.err)
			if !errors.Is(got, tc.err) {
				t.Fatalf("the driver's own error was lost: %v", got)
			}

			var nospace *mailbox.NoSpaceError
			if as := errors.As(got, &nospace); as != tc.noSpace {
				t.Fatalf("errors.As NoSpaceError = %v, want %v", as, tc.noSpace)
			}
			if tc.noSpace && nospace.Folder != "Drafts" {
				t.Errorf("the refusal names folder %q, want Drafts", nospace.Folder)
			}
			if now := refusals(t, "testdriver", tc.reason); now != was+1 {
				t.Errorf("%s counter = %v, want %v", tc.reason, now, was+1)
			}
		})
	}
}

func TestClassifyWriteLetsSuccessThrough(t *testing.T) {
	if err := ClassifyWrite("testdriver", "INBOX", nil); err != nil {
		t.Fatalf("a write that succeeded was classified: %v", err)
	}
}
