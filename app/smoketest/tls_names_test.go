package main

import (
	"net/http"
	"testing"
)

// Verification names come from flags (the stand is dialled by service name or IP);
// once the pinned name is given, the admin APIs never skip the check.
func TestTLSVerificationNames(t *testing.T) {
	defer func(a, b string, c bool) {
		*flagTLSServerName, *flagInternalServerName, *flagInsecure = a, b, c
	}(*flagTLSServerName, *flagInternalServerName, *flagInsecure)

	for _, tc := range []struct {
		name             string
		public, internal string
		insecure         bool
		wantPublic       string
		wantAdmin        string
		wantAdminSkips   bool
	}{
		{"no names: the host", "", "", false, "yarilo-imap-login", "yarilo-backend", false},
		{"public name overrides the host", "mail.example", "", false, "mail.example", "yarilo-backend", false},
		{"pinned name verifies even with -insecure", "", "yarilo-internal", true, "yarilo-imap-login", "yarilo-internal", false},
		{"-insecure without a pinned name skips", "", "", true, "yarilo-imap-login", "yarilo-backend", true},
	} {
		*flagTLSServerName, *flagInternalServerName, *flagInsecure = tc.public, tc.internal, tc.insecure
		if got := publicTLS("yarilo-imap-login").ServerName; got != tc.wantPublic {
			t.Errorf("%s: public ServerName %q, want %q", tc.name, got, tc.wantPublic)
		}
		c, err := adminClient("https://yarilo-backend:9105")
		if err != nil {
			t.Fatal(err)
		}
		cfg := c.Transport.(*http.Transport).TLSClientConfig
		if cfg.ServerName != tc.wantAdmin || cfg.InsecureSkipVerify != tc.wantAdminSkips {
			t.Errorf("%s: admin ServerName %q skip %v, want %q skip %v", tc.name, cfg.ServerName, cfg.InsecureSkipVerify, tc.wantAdmin, tc.wantAdminSkips)
		}
	}
}
