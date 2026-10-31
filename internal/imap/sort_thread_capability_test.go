package imap_test

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

// The server answers SORT and both THREAD algorithms; a client sends them
// only when CAPABILITY names them, so after login it must.
func TestCapabilityNamesSortAndThreadAfterLogin(t *testing.T) {
	c := startACLServer(t, false)
	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatalf("CAPABILITY: %v", err)
	}
	for _, want := range []string{"SORT", "THREAD=REFERENCES", "THREAD=ORDEREDSUBJECT"} {
		if !caps.Has(imaplib.Cap(want)) {
			t.Errorf("CAPABILITY after login does not name %s: %v", want, caps)
		}
	}
}
