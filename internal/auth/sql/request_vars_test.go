package sql_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/emersion/go-sasl"
	_ "modernc.org/sqlite"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	authsql "github.com/yarilomail/yarilo/internal/auth/sql"
	"github.com/yarilomail/yarilo/pkg/sqlvars"
)

// The documented rule: a column per protocol, read through CASE on %s, which
// binds the reference's names (submission, sieve).
const ruleWhere = ` WHERE username = %u
	AND CASE %s WHEN 'imap' THEN imap_enabled WHEN 'submission' THEN submission_enabled ELSE 1 END = 1
	AND (allow_rip = '' OR allow_rip = %{remote_ip})
	AND (allow_lip = '' OR allow_lip = %l)`

// ruleDB holds alice (password pw) with IMAP and submission off, bob limited to
// one client and one local address.
func ruleDB(t *testing.T) string {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "rule.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	creds, err := sasl.GenerateScramSha256Credentials("pw", sasl.MinScramIterations)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE users (username TEXT PRIMARY KEY, password TEXT, scram TEXT, home TEXT,
			imap_enabled INTEGER, submission_enabled INTEGER, allow_rip TEXT, allow_lip TEXT)`,
		`INSERT INTO users VALUES ('alice', '{PLAIN}pw', '{SCRAM-SHA-256}` + sasl.EncodeScramCredentials(creds) + `', '/m/alice', 0, 0, '', '')`,
		`INSERT INTO users VALUES ('bob', '{PLAIN}pw', '', '/m/bob', 1, 1, '10.0.0.1', '10.0.0.2')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

func rulePassdb(t *testing.T, passwordCol string) *authsql.Passdb {
	t.Helper()
	p, err := authsql.New(authsql.Config{
		Driver: "sqlite", DSN: ruleDB(t), SkipSchema: true,
		PasswordQuery: `SELECT ` + passwordCol + ` AS password, home FROM users` + ruleWhere,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() }) //nolint:errcheck
	return p
}

func TestTheRequestValuesReachEveryPassdbLookup(t *testing.T) {
	plain := rulePassdb(t, "password")
	scram := rulePassdb(t, "scram")
	rows := []struct {
		name string
		user string
		v    sqlvars.Vars
		want bool
	}{
		{"imap off", "alice", sqlvars.Vars{Service: "imap"}, false},
		{"pop3 falls to ELSE", "alice", sqlvars.Vars{Service: "pop3"}, true},
		{"smtp binds submission", "alice", sqlvars.Vars{Service: "smtp"}, false},
		{"allowed client and local address", "bob", sqlvars.Vars{Service: "imap", RemoteIP: "10.0.0.1", LocalIP: "10.0.0.2"}, true},
		{"other client", "bob", sqlvars.Vars{Service: "imap", RemoteIP: "10.9.9.9", LocalIP: "10.0.0.2"}, false},
		{"other local address", "bob", sqlvars.Vars{Service: "imap", RemoteIP: "10.0.0.1", LocalIP: "10.8.8.8"}, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			req := &protocol.Request{Username: r.user, Password: "pw", Service: r.v.Service,
				RemoteIP: r.v.RemoteIP, LocalIP: r.v.LocalIP, Fields: protocol.NewFields()}
			res, err := plain.Authenticate(req)
			if err != nil {
				t.Fatal(err)
			}
			if got := res == protocol.ResultOK; got != r.want {
				t.Errorf("password login: ok=%v, want %v", got, r.want)
			}
			found, err := plain.LookupCredentialsFor(r.user, r.v)
			if err != nil || found != r.want {
				t.Errorf("credentials lookup: found=%v err=%v, want %v", found, err, r.want)
			}
			if r.user != "alice" {
				return
			}
			creds, err := scram.LookupSCRAMSha256For(r.user, r.v)
			if err != nil || (creds != nil) != r.want {
				t.Errorf("SCRAM lookup: verifier=%v err=%v, want %v", creds != nil, err, r.want)
			}
		})
	}
	if !plain.UsesAddresses() || !plain.UsesRequest() {
		t.Error("a query reading %r and %l must report it: the cache keys on it")
	}
}

func TestAUserQueryRuleReadsTheProtocol(t *testing.T) {
	u, err := authsql.NewUserdb(authsql.Config{
		Driver: "sqlite", DSN: ruleDB(t), SkipSchema: true,
		UserQuery: `SELECT username, home FROM users WHERE username = %u
			AND CASE %{protocol} WHEN 'imap' THEN imap_enabled ELSE 1 END = 1`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() }) //nolint:errcheck
	if !u.UsesRequest() {
		t.Fatal("a user query on %{protocol} must report it: the auth service runs it on every login")
	}
	for _, r := range []struct {
		service string
		want    bool
	}{{"imap", false}, {"lmtp", true}, {"", true}} {
		ui, err := u.LookupFor("alice", sqlvars.Vars{Service: r.service})
		if err != nil || (ui != nil) != r.want {
			t.Errorf("service %q: found=%v err=%v, want %v", r.service, ui != nil, err, r.want)
		}
	}
}
