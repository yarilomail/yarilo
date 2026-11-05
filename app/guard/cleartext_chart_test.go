package guard_test

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

const cleartextOn = `fts:
  enabled: false
components:
  imapLogin:
    listeners:
      imap: {auth_allow_cleartext: true}
      imaps: {auth_allow_cleartext: true}
  pop3Login:
    listeners:
      pop3: {auth_allow_cleartext: true}
      pop3s: {auth_allow_cleartext: true}
  submissionLogin:
    listeners:
      submission: {auth_allow_cleartext: true}
      submissions: {auth_allow_cleartext: true}
  manageSieveLogin:
    enabled: true
    listeners:
      managesieve: {disablePlainAuth: false}
  jmapLogin:
    enabled: true
    backend_addr: "jmap:10443"
    listeners:
      jmap: {disablePlainAuth: false}
`

const cleartextDefault = `components:
  manageSieveLogin:
    enabled: true
  jmapLogin:
    enabled: true
    backend_addr: "jmap:10443"
`

// Every login block that authenticates render auth_allow_cleartext explicitly: the chart's
// value is the binary's default, and a value set in values reaches the config.
func TestEveryLoginBlockRendersAuthAllowCleartext(t *testing.T) {
	for _, tc := range []struct {
		name, values string
		want         bool
	}{
		{"default", cleartextDefault, config.DefaultAllowCleartext},
		{"set to true", cleartextOn, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := loadRendered(t, mustRender(t, tc.values)).Services
			for _, e := range []struct {
				name string
				svc  *config.ServiceConfig
			}{
				{"imap_login", s.IMAPLogin}, {"imaps_login", s.IMAPSLogin},
				{"pop3_login", s.POP3Login}, {"pop3s_login", s.POP3SLogin},
				{"submission_login", s.SubmissionLogin}, {"submissions_login", s.SubmissionsLogin},
				{"managesieve_login", s.ManageSieveLogin}, {"jmap_login", s.JMAPLogin},
			} {
				switch {
				case e.svc == nil:
					t.Errorf("services.%s is not rendered", e.name)
				case e.svc.AllowCleartext == nil:
					t.Errorf("services.%s.auth_allow_cleartext is not rendered", e.name)
				case *e.svc.AllowCleartext != tc.want:
					t.Errorf("services.%s.auth_allow_cleartext = %v, want %v", e.name, *e.svc.AllowCleartext, tc.want)
				}
			}
		})
	}
}
