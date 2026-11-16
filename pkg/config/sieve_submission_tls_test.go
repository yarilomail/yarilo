package config

import "testing"

// sieve_submission_ssl_verify defaults to true and an explicit false is kept;
// sieve_submission_ssl_ca is read as written.
func TestSieveSubmissionTLSKeys(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantVerify bool
		wantCA     string
	}{
		{"left out", "sieve:\n  sieve_submission_host: relay:587\n", true, ""},
		{"verify off", "sieve:\n  sieve_submission_ssl_verify: false\n", false, ""},
		{"verify on with a CA", "sieve:\n  sieve_submission_ssl_verify: true\n  sieve_submission_ssl_ca: /etc/yarilo/internal-tls/ca.crt\n", true, "/etc/yarilo/internal-tls/ca.crt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tt.body)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Sieve.SubmissionSSLVerify != tt.wantVerify || cfg.Sieve.SubmissionSSLCA != tt.wantCA {
				t.Errorf("verify=%v ca=%q, want %v %q", cfg.Sieve.SubmissionSSLVerify, cfg.Sieve.SubmissionSSLCA, tt.wantVerify, tt.wantCA)
			}
		})
	}
}
