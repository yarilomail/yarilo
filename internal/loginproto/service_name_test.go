package loginproto

import "testing"

// Submission was smtp: across a rollout either name opens it, nothing else
// does, and a listener without a former name keeps its exact match (#2239).
func TestATokenOpensOnlyItsOwnService(t *testing.T) {
	submission := &PreambleListener{ExpectedService: "submission", FormerService: "smtp"}
	imap := &PreambleListener{ExpectedService: "imap"}
	for _, r := range []struct {
		l       *PreambleListener
		service string
		want    bool
	}{
		{submission, "submission", true},
		{submission, "smtp", true},
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
