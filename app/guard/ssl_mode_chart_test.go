package guard_test

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

const sslRequiredValues = `components:
  imapLogin:
    listeners:
      imap: {ssl_mode: required}
  pop3Login:
    listeners:
      pop3: {ssl_mode: required}
  submissionLogin:
    listeners:
      submission: {ssl_mode: required}
  manageSieveLogin:
    enabled: true
    listeners:
      managesieve: {ssl_mode: required}
`

// The four STARTTLS login listeners take ssl_mode from values: starttls by
// default, and required reaches the config as written.
func TestTheSTARTTLSLoginListenersRenderSSLMode(t *testing.T) {
	for _, tc := range []struct {
		name, values, want string
	}{
		{"default", "components:\n  manageSieveLogin:\n    enabled: true\n", "starttls"},
		{"required", sslRequiredValues, "required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := loadRendered(t, mustRender(t, tc.values)).Services
			for _, e := range []struct {
				name string
				svc  *config.ServiceConfig
			}{{"imap_login", s.IMAPLogin}, {"pop3_login", s.POP3Login}, {"submission_login", s.SubmissionLogin}, {"managesieve_login", s.ManageSieveLogin}} {
				if e.svc == nil || e.svc.SSLMode != tc.want {
					t.Errorf("services.%s.ssl_mode = %+v, want %q", e.name, e.svc, tc.want)
				}
			}
		})
	}
}

// ManageSieve's PROXY settings are the general ones: its service block renders
// neither of its old keys.
func TestManageSieveLoginServiceRendersNoOwnProxyKeys(t *testing.T) {
	out := string(mustRender(t, "components:\n  manageSieveLogin:\n    enabled: true\ngeneral:\n  haproxy:\n    enabled: true\n"))
	var block []string
	in := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.TrimSpace(line) == "managesieve_login_service:":
			in = true
		case in && strings.HasPrefix(line, "      "):
			block = append(block, line)
		case in:
			in = false
		}
	}
	if len(block) == 0 {
		t.Fatal("managesieve_login_service not rendered")
	}
	for _, key := range []string{"haproxy_trusted_nets", "haproxy_protocol"} {
		if strings.Contains(strings.Join(block, "\n"), key) {
			t.Errorf("managesieve_login_service renders %s:\n%s", key, strings.Join(block, "\n"))
		}
	}
	cfg := loadRendered(t, []byte(out))
	if !cfg.Services.ManageSieveLogin.HAProxy {
		t.Error("services.managesieve_login.haproxy_protocol does not follow general.haproxy.enabled")
	}
}
