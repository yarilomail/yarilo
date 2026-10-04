package imap_test

import (
	"strings"
	"testing"
)

// SORT and THREAD write the same cache FETCH reads: an encoded word survives
// whoever touched the message first (#2008).
func TestEnvelopeAfterSortKeepsTheEncodedWord(t *testing.T) {
	raw := mailFrom("a@x", "=?utf-8?B?0J/RgNC40LLRltGC?=", "Sun, 1 Mar 2026 10:00:00 +0000", "Alice <a@example.com>")
	conn, rd, _ := threadServerIn(t, []string{raw})

	// The ordering path first: it reads the message and fills the cache.
	if got := sortLine(t, conn, rd, "a3", "SORT (DATE) UTF-8 ALL"); got == "" {
		t.Fatal("SORT answered nothing")
	}

	joined := strings.Join(command(t, conn, rd, "a4", "FETCH 1 (ENVELOPE)"), "\n")
	if !strings.Contains(joined, "=?utf-8?B?") {
		t.Errorf("FETCH after SORT answered %q, want the header's own encoded word", joined)
	}
	if strings.Contains(joined, "Привіт") {
		t.Errorf("FETCH after SORT answered a decoded subject: %q", joined)
	}
}
