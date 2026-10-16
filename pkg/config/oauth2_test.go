package config

import (
	"strings"
	"testing"
)

// Each row is a whole file through Load, so a misspelt key tag reads as the
// setting being absent and the row goes red (#2231).
func TestOAuth2EntryRefusals(t *testing.T) {
	entry := func(body string) string { return "auth:\n  oauth2:\n    - " + body }
	tests := []struct {
		name, body, wantErr string
	}{
		{"grant over https with client id", entry(`oauth2_mode: introspection
      oauth2_introspection_url: https://idp/introspect
      oauth2_grant_url: https://idp/token
      oauth2_client_id: yarilo`), ""},
		{"grant over http", entry(`oauth2_mode: introspection
      oauth2_introspection_url: https://idp/introspect
      oauth2_grant_url: http://idp/token
      oauth2_client_id: yarilo`), "is not https"},
		{"grant without client id", entry(`oauth2_mode: introspection
      oauth2_introspection_url: https://idp/introspect
      oauth2_grant_url: https://idp/token`), "oauth2_client_id is required with oauth2_grant_url"},
		{"grant url without host", entry(`oauth2_mode: introspection
      oauth2_grant_url: "https:///token"
      oauth2_client_id: yarilo`), "is not a URL"},
		{"local keys in mode local", entry(`oauth2_mode: local
      oauth2_local_keys:
        - oauth2_local_key_alg: HS256
          oauth2_local_key_kid: k1
          oauth2_local_key_file: /keys/hs
        - oauth2_local_key_alg: RS256
          oauth2_local_key_file: /keys/rs.pem`), ""},
		{"local keys outside mode local", entry(`oauth2_mode: introspection
      oauth2_introspection_url: https://idp/introspect
      oauth2_local_keys:
        - oauth2_local_key_alg: RS256
          oauth2_local_key_file: /keys/rs.pem`), "only mode local reads them"},
		{"local key with an unknown alg", entry(`oauth2_mode: local
      oauth2_local_keys:
        - oauth2_local_key_alg: none
          oauth2_local_key_file: /keys/x`), `"none" is not one of`},
		{"local key without a file", entry(`oauth2_mode: local
      oauth2_local_keys:
        - oauth2_local_key_alg: RS256`), "oauth2_local_key_file: empty"},
		{"explicit default kid equals an absent one", entry(`oauth2_mode: local
      oauth2_local_keys:
        - oauth2_local_key_alg: RS256
          oauth2_local_key_file: /keys/a.pem
        - oauth2_local_key_alg: RS256
          oauth2_local_key_kid: default
          oauth2_local_key_azp: default
          oauth2_local_key_file: /keys/b.pem`), "is already a key of this entry"},
		{"force after tokeninfo with introspection", entry(`oauth2_mode: tokeninfo
      oauth2_tokeninfo_url: https://idp/tokeninfo
      oauth2_introspection_url: https://idp/introspect
      oauth2_force_introspection: true`), ""},
		{"force after tokeninfo without introspection", entry(`oauth2_mode: tokeninfo
      oauth2_tokeninfo_url: https://idp/tokeninfo
      oauth2_force_introspection: true`), "needs oauth2_introspection_url in mode tokeninfo"},
		{"force with grant in mode introspection", entry(`oauth2_mode: introspection
      oauth2_introspection_url: https://idp/introspect
      oauth2_grant_url: https://idp/token
      oauth2_client_id: yarilo
      oauth2_force_introspection: true`), ""},
		{"force with grant in mode local", entry(`oauth2_mode: local
      oauth2_jwks_url: https://idp/certs
      oauth2_grant_url: https://idp/token
      oauth2_client_id: yarilo
      oauth2_force_introspection: true`), ""},
		{"force in mode introspection without grant", entry(`oauth2_mode: introspection
      oauth2_introspection_url: https://idp/introspect
      oauth2_force_introspection: true`), `does nothing in mode "introspection" without oauth2_grant_url`},
		{"force in mode local without grant", entry(`oauth2_mode: local
      oauth2_jwks_url: https://idp/certs
      oauth2_force_introspection: true`), `does nothing in mode "local" without oauth2_grant_url`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tt.body)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if len(cfg.Auth.OAuth2) != 1 {
					t.Fatalf("entries = %d, want 1", len(cfg.Auth.OAuth2))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load error = %v, want it to say %q", err, tt.wantErr)
			}
		})
	}
}

// The new keys land in the fields the auth service reads.
func TestOAuth2NewKeysLoad(t *testing.T) {
	cfg, err := loadYAML(t, `auth:
  oauth2:
    - oauth2_mode: local
      oauth2_jwks_url: https://idp/certs
      oauth2_grant_url: https://idp/token
      oauth2_client_id: yarilo
      oauth2_force_introspection: true
      oauth2_tls_ca: /etc/ca.crt
      oauth2_local_keys:
        - oauth2_local_key_alg: PS256
          oauth2_local_key_azp: app
          oauth2_local_key_kid: k1
          oauth2_local_key_file: /keys/ps.pem
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := cfg.Auth.OAuth2[0]
	got := []string{e.GrantURL, e.TLSCA, e.LocalKeys[0].Alg, e.LocalKeys[0].Azp, e.LocalKeys[0].Kid, e.LocalKeys[0].File}
	want := []string{"https://idp/token", "/etc/ca.crt", "PS256", "app", "k1", "/keys/ps.pem"}
	if strings.Join(got, "|") != strings.Join(want, "|") || !e.ForceIntrospection {
		t.Errorf("loaded %v force=%v, want %v force=true", got, e.ForceIntrospection, want)
	}
}
