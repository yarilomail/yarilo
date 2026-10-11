package config

import "testing"

// Auth credentials come from the environment (a Secret), never from the
// rendered ConfigMap (#2163).
func TestAuthCredentialsExpandFromTheEnvironment(t *testing.T) {
	t.Setenv("TEST_AUTH_SECRET", "s3cr:et/v@l")
	cfg, err := loadYAML(t, personalNS+`
auth:
  oauth2:
    - oauth2_mode: introspection
      oauth2_introspection_url: "https://idp/introspect"
      oauth2_client_id: yarilo
      oauth2_client_secret: "${TEST_AUTH_SECRET}"
  policy:
    auth_policy_server_url: "https://policy/"
    auth_policy_server_api_header: "${TEST_AUTH_SECRET}"
    auth_policy_hash_nonce: "${TEST_AUTH_SECRET}"
  master_users:
    enabled: true
    masterdb:
      - driver: static
        static_password: "${TEST_AUTH_SECRET}"
      - driver: passwd-file
        passwd_file_path: "/run/${TEST_AUTH_SECRET}"
`)
	if err != nil {
		t.Fatal(err)
	}
	const want = "s3cr:et/v@l"
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"auth.oauth2[0].oauth2_client_secret", cfg.Auth.OAuth2[0].ClientSecret, want},
		{"auth.policy.auth_policy_server_api_header", cfg.Auth.Policy.APIHeader, want},
		{"auth.policy.auth_policy_hash_nonce", cfg.Auth.Policy.HashNonce, want},
		{"auth.master_users.masterdb[0].static_password", cfg.Auth.MasterUsers.Masterdb[0].StaticPassword, want},
		{"auth.master_users.masterdb[1].passwd_file_path", cfg.Auth.MasterUsers.Masterdb[1].PasswdFile, "/run/" + want},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
