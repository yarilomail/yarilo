package config

import "testing"

// The shared config names a certificate only the login pods mount; a session
// binary that reads it dies where its siblings run (#1863). A backend has no
// TLS listener left to ask for one.
func TestASessionBinaryAsksForNoCertificateItCannotHave(t *testing.T) {
	cfg := &Config{}
	cfg.General.SSL.SSLServerCert = "/etc/yarilo/tls/tls.crt" // mounted in the login pod only
	cfg.General.SSL.SSLServerKey = "/etc/yarilo/tls/tls.key"
	cfg.Services = ServicesConfig{
		SubmissionsLogin: &ServiceConfig{Enabled: true, SSLMode: "ssl"},
		Submission:       &ServiceConfig{Enabled: true, SSLMode: "starttls"},
	}
	if err := KeepOnlySessionListener(cfg, RoleSubmission); err != nil {
		t.Fatal(err)
	}
	if !cfg.Services.Submission.Active() {
		t.Fatal("the listener this binary serves was dropped")
	}
	if cfg.Services.SubmissionsLogin != nil {
		t.Error("the backend kept the login proxy's implicit-TLS entry")
	}

	// A listener that does terminate TLS still gets the refusal, rather than a
	// silent plaintext port.
	if _, err := ListenerTLS(cfg, &ServiceConfig{Enabled: true, SSLMode: "ssl"}, "smtp"); err == nil {
		t.Error("a listener that terminates TLS accepted a certificate it cannot read")
	}
}
