package config

import (
	"strings"
	"testing"
)

// A query naming a variable its request never carries fails the load, through
// either spelling of the key and in the masterdb too (#2227).
func TestLoadRefusesAQueryVariableItsRequestLacks(t *testing.T) {
	rows := []struct {
		name, entry, refused string
	}{
		{"protocol and addresses in a password query", "passdb_sql_query: SELECT password FROM t WHERE u = %u AND CASE %s WHEN 'imap' THEN 1 ELSE 1 END = 1 AND %r <> %l", ""},
		{"protocol in a user query", "user_query: SELECT home FROM t WHERE u = %u AND %{protocol} <> 'lmtp'", ""},
		{"client address in a user query", "userdb_sql_query: SELECT home FROM t WHERE u = %u AND %r = '1'", "%r"},
		{"any variable in an iterate query", "iterate_query: SELECT username FROM t WHERE d = %d", "%d"},
		{"unknown long form", "password_query: SELECT password FROM t WHERE u = %{nosuch}", "%{nosuch}"},
	}
	for _, r := range rows {
		for _, section := range []string{"passdb", "master_users:\n    masterdb"} {
			t.Run(r.name+"/"+strings.SplitN(section, ":", 2)[0], func(t *testing.T) {
				indent := "    "
				if section != "passdb" {
					indent = "      "
				}
				body := "auth:\n  " + section + ":\n" + indent + "- driver: sqlite\n" + indent + "  dsn: /tmp/x.db\n" + indent + "  " + r.entry + "\n"
				_, err := loadYAML(t, body)
				switch {
				case r.refused == "" && err != nil:
					t.Errorf("refused: %v", err)
				case r.refused != "" && (err == nil || !strings.Contains(err.Error(), r.refused)):
					t.Errorf("err %v, want one naming %s", err, r.refused)
				}
			})
		}
	}
}
