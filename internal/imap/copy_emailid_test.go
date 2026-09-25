package imap_test

import (
	"fmt"
	"testing"
)

// RFC 8474 §5.1: the copy carries the source's EMAILID, and §5.2 makes the
// THREADID follow it. A fresh GUID here also breaks the search index, where a
// copy is the same document in another folder (#1986).
func TestCopyKeepsTheSourceEmailID(t *testing.T) {
	conn, rd, first, _ := threadIDServer(t, true)

	command(t, conn, rd, "c1", "CREATE Dest")
	command(t, conn, rd, "c2", "UID COPY 1 Dest")
	command(t, conn, rd, "c3", "SELECT Dest")
	lines := command(t, conn, rd, "c4", "FETCH 1 (EMAILID THREADID)")

	// A copy that never landed would leave nothing to FETCH, and an empty
	// answer must not read as agreement.
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
	if got[1] == "" {
		t.Errorf("the copy answers no THREADID; §5.2 ties it to the EMAILID")
	}
}
