package protocol

import "testing"

func TestUsernameFilter(t *testing.T) {
	cases := []struct {
		filter, user string
		want         bool
	}{
		{"", "anyone@x.test", true},
		{"static@d00001.test", "static@d00001.test", true},
		{"static@d00001.test", "nobody@d00001.test", false},
		{"*@d00001.test", "u1@d00001.test", true},
		{"*@d00001.test", "u1@d00002.test", false},
		{"*@d00001.test !u1@*", "u1@d00001.test", false},
		{"*@d00001.test,!u1@*", "u2@d00001.test", true},
		{"!admin@*", "u1@x.test", true},
		{"!admin@*", "admin@x.test", false},
		{"u?@x.test", "u1@x.test", true},
		{"u?@x.test", "u10@x.test", false},
		{"Static@*", "static@x.test", false},
		{"*", "", true},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
	}
	for _, c := range cases {
		if got := ParseUsernameFilter(c.filter).Accepts(c.user); got != c.want {
			t.Errorf("filter %q, user %q = %v, want %v", c.filter, c.user, got, c.want)
		}
	}
}
