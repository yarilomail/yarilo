package config

import "testing"

// The shared config names a certificate only the login pods mount; a session
// binary that reads it dies where its siblings run (#1863).
func TestASessionBinaryAsksForNoCertificateItCannotHave(t *testing.T) {
	cfg := &Config{}
	cfg.General.SSL.SSLServerCert = "/etc/yarilo/tls/tls.crt" // mounted in the login pod only
	cfg.General.SSL.SSLServerKey = "/etc/yarilo/tls/tls.key"
	cfg.Services = ServicesConfig{
		IMAP: &ServiceConfig{Enabled: true}, IMAPS: &ServiceConfig{Enabled: true},
		Submission:  &ServiceConfig{Enabled: true, SSLMode: "starttls"},
		Submissions: &ServiceConfig{Enabled: false},
	}
	KeepOnlySessionListener(cfg, RoleSubmission)

	// The STARTTLS listener it does serve, and the implicit-TLS one it does not.
	if !cfg.Services.Submission.Active() {
		t.Fatal("the listener this binary serves was dropped")
	}
	tlsCfg, err := ListenerTLS(cfg, cfg.Services.Submissions, "smtp")
	if err != nil {
		t.Fatalf("a backend behind a login proxy refused to start: %v", err)
	}
	if tlsCfg != nil {
		t.Error("TLS was built for a listener this binary does not serve")
	}

	// And a deployment that does serve implicit TLS still gets the refusal,
	// rather than a silent plaintext port.
	cfg.Services.Submissions = &ServiceConfig{Enabled: true, SSLMode: "ssl"}
	if _, err := ListenerTLS(cfg, cfg.Services.Submissions, "smtp"); err == nil {
		t.Error("a listener that terminates TLS accepted a certificate it cannot read")
	}
}
