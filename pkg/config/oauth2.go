package config

import (
	"fmt"
	"net/url"
)

// OAuth2LocalKeyAlgs are the algorithms a local key may carry; HS* is a
// shared secret, the rest a public key.
var OAuth2LocalKeyAlgs = map[string]bool{
	"HS256": true, "HS384": true, "HS512": true,
	"RS256": true, "RS384": true, "RS512": true,
	"PS256": true, "PS384": true, "PS512": true,
	"ES256": true, "ES384": true, "ES512": true,
	"EdDSA": true,
}

// validateOAuth2 refuses an entry whose grant, local-key or forced-introspection
// settings could not take effect.
func validateOAuth2(entries []OAuth2Entry) error {
	for i, e := range entries {
		at := fmt.Sprintf("config: auth.oauth2[%d]", i)
		if e.GrantURL != "" {
			u, err := url.Parse(e.GrantURL)
			if err != nil || u.Host == "" {
				return fmt.Errorf("%s.oauth2_grant_url: %q is not a URL", at, e.GrantURL)
			}
			// The grant carries the user's password.
			if u.Scheme != "https" {
				return fmt.Errorf("%s.oauth2_grant_url: %q is not https; the password grant sends the user's password", at, e.GrantURL)
			}
			if e.ClientID == "" {
				return fmt.Errorf("%s: oauth2_client_id is required with oauth2_grant_url", at)
			}
		}
		if len(e.LocalKeys) > 0 && e.Mode != OAuth2ModeLocalJWT {
			return fmt.Errorf("%s.oauth2_local_keys: only mode local reads them, this entry is %q", at, e.Mode)
		}
		seen := map[[3]string]bool{}
		for j, k := range e.LocalKeys {
			kat := fmt.Sprintf("%s.oauth2_local_keys[%d]", at, j)
			if !OAuth2LocalKeyAlgs[k.Alg] {
				return fmt.Errorf("%s.oauth2_local_key_alg: %q is not one of HS256/384/512, RS256/384/512, PS256/384/512, ES256/384/512, EdDSA", kat, k.Alg)
			}
			if k.File == "" {
				return fmt.Errorf("%s.oauth2_local_key_file: empty; the key is read from a file", kat)
			}
			id := [3]string{orDefault(k.Azp), k.Alg, orDefault(k.Kid)}
			if seen[id] {
				return fmt.Errorf("%s: azp %q, alg %s, kid %q is already a key of this entry", kat, id[0], id[1], id[2])
			}
			seen[id] = true
		}
		if e.ForceIntrospection {
			// It acts on a tokeninfo or grant reply; without either nothing reaches it.
			switch {
			case e.Mode == OAuth2ModeTokeninfo && e.IntrospectionURL == "":
				return fmt.Errorf("%s.oauth2_force_introspection: needs oauth2_introspection_url in mode tokeninfo", at)
			case e.Mode != OAuth2ModeTokeninfo && e.GrantURL == "":
				return fmt.Errorf("%s.oauth2_force_introspection: does nothing in mode %q without oauth2_grant_url", at, e.Mode)
			}
		}
	}
	return nil
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
