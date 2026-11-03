package protocol

import (
	"strings"
	"testing"
)

// A userdb mail_max_userip_connections reaches the login proxy's side of the
// wire, under either spelling, so warden can hold the user to it (#2460).
func TestTheUsersOwnLimitCrossesTheWire(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields [][2]string // in the order the chain sets them: passdb, then userdb
		want   string
	}{
		{"absent", nil, ""},
		{"passdb or userdb field", [][2]string{{"mail_max_userip_connections", "3"}}, "3"},
		{"userdb-scoped wins", [][2]string{{"mail_max_userip_connections", "3"}, {"userdb_mail_max_userip_connections", "5"}}, "5"},
		{"0 is kept", [][2]string{{"userdb_mail_max_userip_connections", "0"}}, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFields()
			f.Set("user", "u@x")
			for _, kv := range tc.fields {
				f.Set(kv[0], kv[1])
			}
			if got := extractUserdbValue(f, "mail_max_userip_connections"); got != tc.want {
				t.Fatalf("extractUserdbValue = %q, want %q", got, tc.want)
			}
			res := &AuthResponse{}
			for _, tok := range strings.Split(buildAuthOK("id1", &AuthResponse{Username: "u@x", Fields: f}), "\t")[3:] {
				ApplyAuthOKToken(res, tok)
			}
			if res.MailMaxUserIPConnections != tc.want {
				t.Fatalf("the login side read %q, want %q", res.MailMaxUserIPConnections, tc.want)
			}
		})
	}
}
