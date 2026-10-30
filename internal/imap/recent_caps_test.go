package imap

import (
	"slices"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

// Only a \Recent message asks for the connection's capabilities, a copy under
// its mutex, and a session that enabled IMAP4rev2 asks once (#2404).
func TestWithRecentAsksForTheCapabilitiesOnlyWhenItMatters(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rev2      bool
		uid       uint32
		wantRecnt bool
		wantAsks  int
	}{
		{"a message the session does not hold", false, 9, false, 0},
		{"a \\Recent message, IMAP4rev1", false, 3, true, 3},
		{"a \\Recent message, IMAP4rev2", true, 3, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asks := 0
			caps := imaplib.CapSet{}
			if tc.rev2 {
				caps[imaplib.CapIMAP4rev2] = struct{}{}
			}
			s := &session{enabledCaps: func() imaplib.CapSet { asks++; return caps }}
			s.recent.AddNum(3)
			for range 3 {
				got := s.withRecent([]imaplib.Flag{imaplib.FlagSeen}, tc.uid)
				if has := slices.Contains(got, flagRecent); has != tc.wantRecnt {
					t.Fatalf("\\Recent in %v: %v, want %v", got, has, tc.wantRecnt)
				}
			}
			if asks != tc.wantAsks {
				t.Errorf("asked for the capabilities %d times over three lines, want %d", asks, tc.wantAsks)
			}
		})
	}
}
