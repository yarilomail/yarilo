package imap_test

import (
	"fmt"
	"testing"
)

// RFC 8474 §5.1: the copy carries the source's EMAILID, and §5.2 its
// THREADID. A fresh GUID here also splits the search document (#1986).
func TestCopyKeepsTheSourceEmailID(t *testing.T) {
	conn, rd, first, _ := threadIDServer(t, true)
	source := fetchIDs(t, conn, rd)[1]

	command(t, conn, rd, "c1", "CREATE Dest")
	command(t, conn, rd, "c2", "UID COPY 1 Dest")
	command(t, conn, rd, "c3", "SELECT Dest")
	lines := command(t, conn, rd, "c4", "FETCH 1 (EMAILID THREADID)")

	// Nothing to FETCH must not read as agreement.
	var got [2]string
	for _, line := range lines {
		var seq int
		if _, err := fmt.Sscanf(line, "* %d FETCH", &seq); err != nil {
			continue
		}
		got = [2]string{fieldAfter(line, "EMAILID"), fieldAfter(line, "THREADID")}
	}
	if got[0] == "" {
		t.Fatalf("the destination holds no message after COPY: %v", lines)
	}
	if got[0] != first {
		t.Errorf("the copy answers EMAILID %q, want the source's %q", got[0], first)
	}
	// §5.2: every message with one EMAILID carries one THREADID, so a copy
	// with a thread of its own is as wrong as a copy with its own EMAILID.
	if got[1] != source[1] {
		t.Errorf("the copy answers THREADID %q, want the source's %q", got[1], source[1])
	}
}
