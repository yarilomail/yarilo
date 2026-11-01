package config

import (
	"crypto/tls"
	"fmt"
	"slices"
	"strings"
)

// SessionRole names the one listener a session binary serves. Everything else
// belongs to a sibling process or to the login proxy in front of it.
type SessionRole string

// The session roles, by listener.
const (
	RoleIMAP        SessionRole = "imap"
	RolePOP3        SessionRole = "pop3"
	RoleLMTP        SessionRole = "lmtp"
	RoleManageSieve SessionRole = "managesieve"
	RoleSubmission  SessionRole = "submission"
	RoleJMAP        SessionRole = "jmap"
)

// KeepOnlySessionListener leaves the one entry this backend reads and drops
// the rest; it refuses a config without that entry (#2415).
func KeepOnlySessionListener(cfg *Config, role SessionRole) error {
	if err := RequireListeners(cfg, string(role)); err != nil {
		return err
	}
	for _, e := range cfg.Services.Entries() {
		if e.Name != string(role) {
			*e.Svc = nil
		}
	}
	return nil
}

// KeepBackendListeners leaves the single binary the backend entries the config
// has and drops the login proxies'; it refuses a config with none of them.
func KeepBackendListeners(cfg *Config) error {
	backends := []string{string(RoleIMAP), string(RolePOP3), string(RoleLMTP),
		string(RoleManageSieve), string(RoleSubmission), string(RoleJMAP)}
	kept := 0
	for _, e := range cfg.Services.Entries() {
		if !slices.Contains(backends, e.Name) {
			*e.Svc = nil
		} else if *e.Svc != nil {
			kept++
		}
	}
	if kept == 0 {
		return fmt.Errorf("config: none of services.%s is set; the yarilo binary listens only on backend entries",
			strings.Join(backends, ", services."))
	}
	return nil
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
