package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// ssl_mode required is accepted only on a STARTTLS login listener and never
// together with auth_allow_cleartext true.
func TestSSLRequiredIsCheckedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantErr string
	}{
		{"imap_login", "services:\n  imap_login:\n    ssl_mode: required\n", ""},
		{"managesieve_login, cleartext off said", "services:\n  managesieve_login:\n    ssl_mode: required\n    auth_allow_cleartext: false\n", ""},
		{"with cleartext allowed", "services:\n  pop3_login:\n    ssl_mode: required\n    auth_allow_cleartext: true\n", "auth_allow_cleartext: true"},
		{"with the pre-beta spelling allowing it", "services:\n  submission_login:\n    ssl_mode: required\n    disable_plaintext_auth: false\n", "auth_allow_cleartext: true"},
		{"on an implicit-TLS listener", "services:\n  imaps_login:\n    ssl_mode: required\n", "STARTTLS login listeners only"},
		{"on a session backend", "services:\n  imap:\n    ssl_mode: required\n", "STARTTLS login listeners only"},
		{"in capitals", "services:\n  imap_login:\n    ssl_mode: REQUIRED\n    auth_allow_cleartext: true\n", "auth_allow_cleartext: true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.body)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// required with no certificate could never take a login: refused like ssl.
func TestSSLRequiredWithoutACertificateIsRefused(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		refused bool
	}{{"required", true}, {"ssl", true}, {"starttls", false}, {"no", false}} {
		t.Run(tc.mode, func(t *testing.T) {
			_, err := CheckListenerTLS("services.imap_login", &ServiceConfig{Enabled: true, SSLMode: tc.mode}, SSLConfig{})
			if (err != nil) != tc.refused {
				t.Errorf("err = %v, refused want %v", err, tc.refused)
			}
		})
	}
	if _, err := CheckListenerTLS("services.imap_login", &ServiceConfig{Enabled: true, SSLMode: "required"},
		SSLConfig{SSLServerCert: "c", SSLServerKey: "k"}); err != nil {
		t.Errorf("required with a certificate refused: %v", err)
	}
}

// ManageSieve takes the general PROXY networks; its own key is the old
// spelling, used only when the general one is empty, and warned about.
func TestManageSieveHAProxyNets(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
		warns      []string
	}{
		{"general only", "general:\n  haproxy:\n    haproxy_trusted_networks: [10.0.0.0/8]\n",
			[]string{"10.0.0.0/8"}, nil},
		{"old key only", "managesieve_login_service:\n  haproxy_trusted_nets: [192.0.2.0/24]\n",
			[]string{"192.0.2.0/24"}, []string{"managesieve_login_service.haproxy_trusted_nets"}},
		{"both, the general one wins", "general:\n  haproxy:\n    haproxy_trusted_networks: [10.0.0.0/8]\nmanagesieve_login_service:\n  haproxy_trusted_nets: [192.0.2.0/24]\n",
			[]string{"10.0.0.0/8"}, []string{"managesieve_login_service.haproxy_trusted_nets", "both set; using the general one"}},
		{"old protocol switch, the general default nets", "managesieve_login_service:\n  haproxy_protocol: true\n",
			Defaults().General.HAProxy.HAProxyTrustedNetworks, []string{"managesieve_login_service.haproxy_protocol"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			cfg, err := loadYAML(t, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(cfg.ManageSieveHAProxyNets(), ","); got != strings.Join(tc.want, ",") {
				t.Errorf("nets = %q, want %q", got, tc.want)
			}
			for _, w := range tc.warns {
				if !strings.Contains(buf.String(), w) {
					t.Errorf("no warning naming %q:\n%s", w, buf.String())
				}
			}
			if len(tc.warns) == 0 && strings.Contains(buf.String(), "managesieve_login_service.") {
				t.Errorf("a warning about the old keys on a config without them:\n%s", buf.String())
			}
		})
	}
}
