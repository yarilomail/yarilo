package main

import (
	"strings"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// The FTS document row reads the index through the admin API: without it the
// row is a skip that names the flag, not a failure about a URL (#2456).
func TestTheFTSDocumentRowNeedsTheAdminAPI(t *testing.T) {
	setFlag(t, flagFTSUser, "u1@d.test")
	setFlag(t, flagFTSPass, "pw")
	for _, tc := range []struct {
		name     string
		api      string
		runnable bool
	}{
		{"no admin API", "", false},
		{"with the admin API", "https://yarilo-backend:9105", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setFlag(t, flagBackendAPI, tc.api)
			var row *check
			for _, c := range register() {
				if strings.HasPrefix(c.name, "imap FTS document is the message") {
					row = &c
					break
				}
			}
			if row == nil {
				t.Fatal("the FTS document row is not registered")
			}
			if runnable := row.fn != nil; runnable != tc.runnable {
				t.Fatalf("runnable = %v, want %v (skip %q)", runnable, tc.runnable, row.skip)
			}
			if !tc.runnable && !strings.Contains(row.skip, "-backend-api") {
				t.Errorf("skip %q does not name -backend-api", row.skip)
			}
		})
	}
}

// The delivered copy is told from the draft by a Received header, which any
// MTA on the way adds, whatever it is called: the stand's relay-mta, or a
// Postfix named freebsd (#2456).
func TestTheDeliveredCopyIsFoundWhateverTheRelayIsCalled(t *testing.T) {
	const marker = "xjmapsubmit-1"
	draft := "From: <u@d.test>\r\nSubject: " + marker + "\r\n\r\nbody\r\n"
	c, err := imapserver.ParseSearchCriteria(deliveredCriteria(marker, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"through the stand's relay", "Received: from relay-mta.yarilo-relay by mx\r\n" + draft, true},
		{"through a Postfix named freebsd", "Received: from localhost by freebsd (Postfix)\r\n" + draft, true},
		{"the imported draft", draft, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := imapserver.MatchMessage(1, imaplib.UID(1), time.Now(), int64(len(tc.raw)), nil, []byte(tc.raw), c)
			if got != tc.want {
				t.Errorf("matched = %v, want %v", got, tc.want)
			}
		})
	}
}
