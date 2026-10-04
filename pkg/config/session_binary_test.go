package config

import "testing"

// A session binary keeps the listener it serves and drops the rest: the
// certificate lives in the login pod, not here (#1863).
func TestASessionBinaryKeepsOnlyItsOwnListener(t *testing.T) {
	on := func() *ServiceConfig { return &ServiceConfig{Enabled: true} }
	full := func() *Config {
		c := &Config{}
		c.Services = ServicesConfig{
			IMAP: on(), IMAPS: on(), POP3: on(), POP3S: on(), LMTP: on(),
			Submission: on(), Submissions: on(), ManageSieve: on(),
			ManageSieveBE: on(), JMAP: on(), JMAPBE: on(),
		}
		return c
	}
	tests := []struct {
		role SessionRole
		kept []string
	}{
		{RoleIMAP, []string{"imap"}},
		{RolePOP3, []string{"pop3"}},
		{RoleLMTP, []string{"lmtp"}},
		{RoleManageSieve, []string{"managesieve", "managesieve_be"}},
		{RoleSubmission, []string{"submission", "submissions"}},
	}
	for _, tc := range tests {
		t.Run(string(tc.role), func(t *testing.T) {
			cfg := full()
			KeepOnlySessionListener(cfg, tc.role)
			active := map[string]bool{
				"imap": cfg.Services.IMAP.Active(), "imaps": cfg.Services.IMAPS.Active(),
				"pop3": cfg.Services.POP3.Active(), "pop3s": cfg.Services.POP3S.Active(),
				"lmtp": cfg.Services.LMTP.Active(), "submission": cfg.Services.Submission.Active(),
				"submissions": cfg.Services.Submissions.Active(), "managesieve": cfg.Services.ManageSieve.Active(),
				"managesieve_be": cfg.Services.ManageSieveBE.Active(), "jmap": cfg.Services.JMAP.Active(),
				"jmap_be": cfg.Services.JMAPBE.Active(),
			}
			want := map[string]bool{}
			for _, k := range tc.kept {
				want[k] = true
			}
			for name, isOn := range active {
				if isOn != want[name] {
					t.Errorf("%s: %s active=%v, want %v", tc.role, name, isOn, want[name])
				}
			}
		})
	}
}

// The one that started this: no TLS-terminating listener survives for a binary
// whose certificate is held by the proxy in front of it (#1863).
func TestNoSessionBinaryKeepsATLSTerminatingListener(t *testing.T) {
	for _, role := range []SessionRole{RoleIMAP, RolePOP3, RoleLMTP, RoleManageSieve} {
		cfg := &Config{}
		cfg.Services = ServicesConfig{IMAPS: &ServiceConfig{Enabled: true}, POP3S: &ServiceConfig{Enabled: true}}
		KeepOnlySessionListener(cfg, role)
		if cfg.Services.IMAPS.Active() || cfg.Services.POP3S.Active() {
			t.Errorf("%s kept an implicit-TLS listener", role)
		}
	}
}
