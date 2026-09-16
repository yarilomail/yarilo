package config

import "crypto/tls"

// SessionRole names the one listener a session binary serves. Everything else
// belongs to a sibling process or to the login proxy in front of it.
type SessionRole string

const (
	RoleIMAP        SessionRole = "imap"
	RolePOP3        SessionRole = "pop3"
	RoleLMTP        SessionRole = "lmtp"
	RoleManageSieve SessionRole = "managesieve"
	RoleSubmission  SessionRole = "submission"
)

// KeepOnlySessionListener leaves the listener this binary serves and drops the
// rest, the TLS-terminating ones included: the login proxy in front holds the
// client certificate, and a backend that reads general.ssl dies on a file it
// was never given (#1863).
//
// One function rather than the same seven assignments in five main.go files:
// the fifth binary did not have them, which is how this was found.
func KeepOnlySessionListener(cfg *Config, role SessionRole) {
	s := &cfg.Services
	all := []**ServiceConfig{&s.IMAP, &s.IMAPS, &s.POP3, &s.POP3S, &s.LMTP,
		&s.Submission, &s.Submissions, &s.ManageSieve, &s.ManageSieveBE, &s.JMAP, &s.JMAPBE}
	var kept []**ServiceConfig
	switch role {
	case RoleIMAP:
		kept = []**ServiceConfig{&s.IMAP}
	case RolePOP3:
		kept = []**ServiceConfig{&s.POP3}
	case RoleLMTP:
		kept = []**ServiceConfig{&s.LMTP}
	case RoleManageSieve:
		kept = []**ServiceConfig{&s.ManageSieve, &s.ManageSieveBE}
	case RoleSubmission:
		// Both: submissions is the chart's own knob for implicit TLS on the
		// backend, and a deployment that turns it on mounts the certificate.
		kept = []**ServiceConfig{&s.Submission, &s.Submissions}
	}
	keeping := map[**ServiceConfig]bool{}
	for _, k := range kept {
		keeping[k] = true
	}
	for _, p := range all {
		if !keeping[p] {
			*p = nil
		}
	}
}

// ListenerTLS builds the server TLS for one listener, or nothing when that
// listener is not served here: a process behind a login proxy has no
// certificate to read, and reading one it was never given is fatal (#1863).
func ListenerTLS(cfg *Config, svc *ServiceConfig, alpn ...string) (*tls.Config, error) {
	if !svc.Active() {
		return nil, nil
	}
	ssl := cfg.ResolveSSL(svc)
	if ssl.SSLServerCert == "" || ssl.SSLServerKey == "" {
		return nil, nil
	}
	t, err := BuildTLSConfig(ssl)
	if err != nil {
		return nil, err
	}
	if len(alpn) > 0 {
		t.NextProtos = alpn
	}
	return t, nil
}
