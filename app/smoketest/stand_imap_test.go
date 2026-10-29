package main

import "testing"

// The IMAP rows against yarilo's own IMAP server.
func TestIMAPRowsAgainstTheServer(t *testing.T) {
	s := startStand(t)
	s.withIMAP(t)
	s.withLMTP(t)
	s.withPOP3(t)
	rows := []struct {
		name string
		fn   func() error
	}{
		{"login", func() error { return checkIMAPLogin(standUser, standPass) }},
		{"fetch envelope on an empty INBOX", func() error { return checkFetchEnvelope(standUser, standPass) }},
		{"acl round trip", func() error { return checkACL(standUser, standPass) }},
		{"quota", func() error { return checkQuota(standUser, standPass) }},
		{"one lmtp delivery seen by every reader", func() error { return checkConsistencyReaders(standUser, standPass) }},
		{"pop3 maildrop cycle", func() error { return checkPOP3Cycle(standUser, standPass) }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if err := r.fn(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := checkIMAPLogin(standUser, "wrong"); err == nil {
		t.Error("a wrong password logged in")
	}
}
