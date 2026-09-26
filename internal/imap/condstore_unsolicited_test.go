package imap_test

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Another session's flag change arrives with MODSEQ once CONDSTORE (or QRESYNC)
// is enabled, and without it before (RFC 7162 3.2.4, #2046).
func TestAnUnsolicitedFlagChangeCarriesItsModseq(t *testing.T) {
	for _, tc := range []struct {
		name, enable string
		want         bool
	}{
		{"CONDSTORE enabled", "ENABLE CONDSTORE", true},
		{"QRESYNC enabled", "ENABLE QRESYNC", true},
		{"nothing enabled", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, rd := virtualServer(t, map[string]string{},
				func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
					saveInto(t, box, ui, "INBOX", 1, "one", nil)
				})
			if tc.enable != "" {
				command(t, conn, rd, "a2", tc.enable)
			}
			existsCount(t, conn, rd, "a3", "INBOX")
			other, ord := loginTo(t, lastVirtualAddr)
			existsCount(t, other, ord, "b2", "INBOX")
			reply := fetchLine(t, other, ord, "b3", `STORE 1 +FLAGS (\Flagged)`)

			got := linesWith(command(t, conn, rd, "a4", "NOOP"), " FETCH ")
			if len(got) != 1 {
				t.Fatalf("NOOP answered %v, want the one flag change", got)
			}
			if has := strings.Contains(got[0], "MODSEQ"); has != tc.want {
				t.Fatalf("NOOP answered %q, MODSEQ present %v, want %v", got[0], has, tc.want)
			}
			if tc.want {
				stored := linesWith(command(t, other, ord, "b4", "FETCH 1 (MODSEQ)"), " FETCH ")
				if len(stored) != 1 || modseqOf(t, got[0]) != modseqOf(t, stored[0]) {
					t.Errorf("NOOP reported %q, the record holds %v (store answered %q)", got[0], stored, reply)
				}
			}
		})
	}
}
