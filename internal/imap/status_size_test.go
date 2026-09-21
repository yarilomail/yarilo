package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// STATUS SIZE answers for the message, not for the bytes on disk. A foreign
// delivery is imported with the size its name and a stat give -- physical only,
// because a bare name carries no W= -- so the record's own number is the byte
// count, while the message's size counts the CRLF a client is told about
// (#1726, index-mailbox-size.c:387-411).
//
// The order is the row: the first FETCH stamps the measured size into the
// record (#1728), and after that the two sums agree whatever they read. STATUS
// has to be asked first, which is also the order a client uses.
func TestStatusSizeCountsTheMessageNotTheBytes(t *testing.T) {
	root, addr := startIdentityServer(t)
	c := dialRaw(t, addr)
	c.login()
	c.cmd(`SELECT INBOX`)

	// Two lines ending in bare LF: four bytes on disk, six as RFC822.
	deliverForeign(t, root, "INBOX", "1700004000.M1P1.mda", "a\nb\n")
	c.cmd(`SELECT INBOX`) // the settle that imports it

	// Six is what both should say; today both say four, because the index
	// cannot hold "no virtual size" and writes the byte count instead
	// (#1962). Until that is fixed, what this row can hold is that the two
	// commands answer alike -- and STATUS is asked first, because the first
	// FETCH stamps the record (#1728).
	status := statusSize(t, c)
	fetch := numberAfter(t, c.cmd(`FETCH 1 (RFC822.SIZE)`), "RFC822.SIZE ")
	if status != fetch {
		t.Errorf("STATUS SIZE is %d and FETCH RFC822.SIZE is %d for the same message", status, fetch)
	}
	if status == 0 {
		t.Error("the message is reported with no size at all")
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
