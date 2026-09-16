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
// rest: the login proxy in front holds the client certificate (#1863).
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

// ListenerTLS builds server TLS for a listener that terminates it, and nothing
// for one this process does not serve (#1863).
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
