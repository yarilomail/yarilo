package config

import (
	"regexp"
	"strings"
	"testing"
)

// notSecret names keys whose name sounds secret and whose value is not one.
var notSecret = map[string]string{
	"nopassword":                           "a switch",
	"passdb_default_password_scheme":       "a scheme name",
	"password_query":                       "an SQL query; credentials live in dsn",
	"tokeninfo_url":                        "an endpoint",
	"oauth2_tokeninfo_url":                 "an endpoint",
	"token_expire_grace_seconds":           "a duration",
	"oauth2_token_expire_grace_seconds":    "a duration",
	"oauth2_local_key_alg":                 "an algorithm name",
	"oauth2_local_key_azp":                 "a client id",
	"oauth2_local_key_kid":                 "a key id, published with the key",
	"oauth2_local_key_file":                "a path to a public key",
	"key_prefix":                           "a Redis key prefix",
	"key":                                  "internal_tls.key: a path to the key file",
	"tls_key":                              "a path to the key file",
	"tls_alt_key":                          "a path to the key file",
	"ssl_server_key_file":                  "a path to the key file",
	"ssl_server_alt_key_file":              "a path to the key file",
	"public_key":                           "a path to a public key",
	"private_keys":                         "a path to the key file",
	"reference_keys":                       "paths to the key files",
	"language_tokenizer_generic_algorithm": "a tokenizer setting",
}

var secretName = regexp.MustCompile(`password|secret|token|key`)

// TestEverySecretSoundingKeyIsTaggedOrExplained: a credential is hidden by its
// tag, never by its name; a name that sounds like one and is not says why.
func TestEverySecretSoundingKeyIsTaggedOrExplained(t *testing.T) {
	tagged := 0
	for _, s := range Walk(Defaults(), true) {
		name := s.path[len(s.path)-1].name
		if s.Secret {
			tagged++
			continue
		}
		if !secretName.MatchString(name) || strings.HasPrefix(name, "fts_language_tokenizer") || strings.HasPrefix(name, "language_tokenizer") {
			continue
		}
		if _, ok := notSecret[name]; !ok {
			t.Errorf("%s: tag it secret:\"true\", or say in notSecret why its value is not a credential", s.Key)
		}
	}
	if tagged == 0 {
		t.Fatal("no key is tagged secret")
	}
}
