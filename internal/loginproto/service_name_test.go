package loginproto

import "testing"

// A token opens only the service it was verified for: smtp, Submission's name
// before 2.4.3 and Postfix's now, does not open Submission (#2239).
func TestATokenOpensOnlyItsOwnService(t *testing.T) {
	submission := &PreambleListener{ExpectedService: "submission"}
	imap := &PreambleListener{ExpectedService: "imap"}
	for _, r := range []struct {
		l       *PreambleListener
		service string
		want    bool
	}{
		{submission, "submission", true},
		{submission, "smtp", false},
		{submission, "imap", false},
		{submission, "", false},
		{imap, "imap", true},
		{imap, "smtp", false},
		{imap, "", false},
	} {
		if got := r.l.serviceAccepted(r.service); got != r.want {
			t.Errorf("%s listener, token for %q: accepted=%v, want %v", r.l.ExpectedService, r.service, got, r.want)
		}
	}
}
