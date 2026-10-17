package sqlvars

import (
	"reflect"
	"strings"
	"testing"
)

func TestSubstituteBindsEveryVariable(t *testing.T) {
	v := Vars{Service: "managesieve", RemoteIP: "192.0.2.1", LocalIP: "198.51.100.2"}
	rows := []struct {
		name, driver, query string
		wantQuery           string
		wantArgs            []any
	}{
		{"short forms", "mysql", "SELECT 1 WHERE u=%u AND n=%n AND d=%d AND s=%s AND r=%r AND l=%l",
			"SELECT 1 WHERE u=? AND n=? AND d=? AND s=? AND r=? AND l=?",
			[]any{"alice@example.com", "alice", "example.com", "sieve", "192.0.2.1", "198.51.100.2"}},
		{"long forms, postgres", "postgres", "x=%{user} %{username} %{domain} %{protocol} %{service} %{remote_ip} %{rip} %{local_ip} %{lip}",
			"x=$1 $2 $3 $4 $5 $6 $7 $8 $9",
			[]any{"alice@example.com", "alice", "example.com", "sieve", "sieve", "192.0.2.1", "192.0.2.1", "198.51.100.2", "198.51.100.2"}},
		{"quoted text is left alone", "sqlite", "SELECT '%s %{protocol}' WHERE s=%s",
			"SELECT '%s %{protocol}' WHERE s=?", []any{"sieve"}},
		{"an unknown short form is SQL's", "sqlite", "SELECT a %x b WHERE u=%u",
			"SELECT a %x b WHERE u=?", []any{"alice@example.com"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			q, args := Substitute(r.driver, r.query, "alice@example.com", v)
			if q != r.wantQuery || !reflect.DeepEqual(args, r.wantArgs) {
				t.Errorf("got %q %v\nwant %q %v", q, args, r.wantQuery, r.wantArgs)
			}
		})
	}
}

func TestProtocolNameIsTheReferences(t *testing.T) {
	for in, want := range map[string]string{
		"submission": "submission", "smtp": "smtp", "managesieve": "sieve",
		"imap": "imap", "pop3": "pop3", "lmtp": "lmtp", "jmap": "jmap", "": "",
	} {
		if got := ProtocolName(in); got != want {
			t.Errorf("ProtocolName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A variable a query kind is never given is refused, never bound as empty.
func TestCheckRefusesWhatTheRequestDoesNotCarry(t *testing.T) {
	rows := []struct {
		kind    Kind
		query   string
		refused string // a substring of the error; empty means accepted
	}{
		{Password, "WHERE u=%u AND s=%s AND r=%r AND l=%{lip}", ""},
		{User, "WHERE u=%u AND s=%{protocol}", ""},
		{User, "WHERE u=%u AND r=%r", "%r"},
		{User, "WHERE u=%u AND l=%{local_ip}", "%{local_ip}"},
		{Iterate, "SELECT username FROM t WHERE d=%d", "%d"},
		{Iterate, "SELECT username FROM t", ""},
		{Password, "WHERE u=%{nosuch}", "unknown variable %{nosuch}"},
		{Password, "WHERE u=%{user", "unterminated"},
		{User, "SELECT '%r' WHERE u=%u", ""},
	}
	for _, r := range rows {
		err := Check(r.kind, r.query)
		switch {
		case r.refused == "" && err != nil:
			t.Errorf("%s %q refused: %v", r.kind, r.query, err)
		case r.refused != "" && (err == nil || !strings.Contains(err.Error(), r.refused)):
			t.Errorf("%s %q: err %v, want one naming %q", r.kind, r.query, err, r.refused)
		}
	}
}

func TestUsesReportsTheRequestValues(t *testing.T) {
	for _, r := range []struct {
		query            string
		request, address bool
	}{
		{"WHERE u=%u AND d=%d", false, false},
		{"WHERE s=%s", true, false},
		{"WHERE r=%{rip}", true, true},
		{"WHERE l=%l", true, true},
		{"WHERE x='%s'", false, false},
	} {
		if got := UsesRequest(r.query); got != r.request {
			t.Errorf("UsesRequest(%q) = %v", r.query, got)
		}
		if got := UsesAddresses(r.query); got != r.address {
			t.Errorf("UsesAddresses(%q) = %v", r.query, got)
		}
	}
}
