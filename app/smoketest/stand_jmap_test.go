package main

import "testing"

// The JMAP rows against yarilo's own JMAP backend, on the store IMAP and LMTP
// share; the rows that find a message by its subject need FTS (stand_fts_test.go).
func TestJMAPRowsAgainstTheServer(t *testing.T) {
	s := startStand(t)
	s.withIMAP(t)
	s.withLMTP(t)
	s.withJMAP(t)
	s.withBackendAPI(t, nil)
	rows := []struct {
		name string
		fn   func() error
	}{
		{"refuses anonymous access", checkJMAPUnauthenticated},
		{"session", checkJMAPSession},
		{"batch", checkJMAPBatch},
		{"mailboxes", checkJMAPMailboxes},
		{"mailbox query", checkJMAPMailboxQuery},
		{"round trip", checkJMAPRoundTrip},
		{"download isolation", checkJMAPDownloadIsolation},
		{"header forms", checkJMAPHeaderForms},
		{"email discovery", func() error {
			if err := deliverStandProbe(); err != nil {
				return err
			}
			return checkJMAPEmailDiscovery()
		}},
		{"imap<->jmap quota", func() error { return checkConsistencyQuotaJMAP(standUser) }},
		{"imap<->admin API quota", func() error { return checkConsistencyQuota(standUser) }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if err := r.fn(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// deliverStandProbe puts one message in the stand user's INBOX over LMTP.
func deliverStandProbe() error {
	return deliverConsistencyProbe(standUser, "xstand-probe")
}
