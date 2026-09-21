package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One message, two commands, one number: STATUS SIZE and FETCH RFC822.SIZE
// answer from the same place, and a record that does not carry both sizes is
// filled from the store either way (#1726, index-mailbox-size.c:387-411).
//
// The row pins that agreement; it does not prove the source, because after a
// settle the record carries both numbers and the two paths agree whatever they
// read (#1959).
func TestStatusSizeAndFetchAgreeOnAForeignDelivery(t *testing.T) {
	root, addr := startIdentityServer(t)
	c := dialRaw(t, addr)
	c.login()
	c.cmd(`SELECT INBOX`)

	// What a foreign MDA leaves: a bare name with no sizes in it, and a body
	// whose lines end in LF alone.
	deliverForeign(t, root, "INBOX", "1700004000.M1P1.mda", "From: a@b\nSubject: lf\n\nbody\n")
	c.cmd(`SELECT INBOX`) // the settle that imports it

	fetch := c.cmd(`FETCH 1 (RFC822.SIZE)`)
	want := numberAfter(t, fetch, "RFC822.SIZE ")
	if want == 0 {
		t.Fatalf("FETCH reports no size, so the row proves nothing:\n%s", fetch)
	}
	if got := statusSize(t, c); got != want {
		t.Errorf("STATUS SIZE is %d and FETCH RFC822.SIZE is %d for the same message", got, want)
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
