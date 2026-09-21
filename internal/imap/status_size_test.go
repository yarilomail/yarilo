package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// STATUS SIZE answers for the message, not for the bytes on disk: a bare name
// carries no W=, and the RFC822 form counts line endings (#1726, #1962).
//
// STATUS is asked first, because the first FETCH stamps the record (#1728) and
// after that both sums agree whatever they read.
func TestStatusSizeCountsTheMessageNotTheBytes(t *testing.T) {
	root, addr := startIdentityServer(t)
	c := dialRaw(t, addr)
	c.login()
	c.cmd(`SELECT INBOX`)

	// Two lines ending in bare LF: four bytes on disk, six as RFC822.
	deliverForeign(t, root, "INBOX", "1700004000.M1P1.mda", "a\nb\n")
	c.cmd(`SELECT INBOX`) // the settle that imports it

	// Six: four bytes on disk, two line endings a client is told about.
	const wantVirtual = 6
	if got := statusSize(t, c); got != wantVirtual {
		t.Errorf("STATUS SIZE is %d, want %d: it answered with the bytes on disk", got, wantVirtual)
	}
	fetch := c.cmd(`FETCH 1 (RFC822.SIZE)`)
	if got := numberAfter(t, fetch, "RFC822.SIZE "); got != wantVirtual {
		t.Errorf("FETCH RFC822.SIZE is %d, want %d:\n%s", got, wantVirtual, fetch)
	}
}

// deliverForeign writes a message into the arrival directory the way an
// external MDA does: no sizes in the name, and nothing recorded anywhere.
func deliverForeign(t *testing.T, root, folder, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(folderDir(root, folder), "new", name), []byte(body), 0o600); err != nil {
		t.Fatalf("the MDA's delivery: %v", err)
	}
}

func statusSize(t *testing.T, c *rawConn) int {
	t.Helper()
	return numberAfter(t, c.cmd(`STATUS INBOX (MESSAGES SIZE)`), "SIZE ")
}

func numberAfter(t *testing.T, out, key string) int {
	t.Helper()
	i := strings.Index(out, key)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", key, out)
	}
	rest := out[i+len(key):]
	n := 0
	for _, ch := range rest {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + int(ch-'0')
	}
	return n
}
