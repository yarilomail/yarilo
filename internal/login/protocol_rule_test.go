package login

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	authsql "github.com/yarilomail/yarilo/v2/internal/auth/sql"
)

// ruleAuth serves a SQL passdb whose query turns protocols off per column, the
// documented CASE on %s; alice has the protocols named in off disabled.
func ruleAuth(t *testing.T, off map[string]bool) string {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "rule.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	flag := func(p string) int {
		if off[p] {
			return 0
		}
		return 1
	}
	for _, q := range []string{
		`CREATE TABLE users (username TEXT, password TEXT, imap INTEGER, pop3 INTEGER, submission INTEGER)`,
		`INSERT INTO users VALUES ('alice', '{PLAIN}secret', ?, ?, ?)`,
	} {
		if _, err := db.Exec(q, flag("imap"), flag("pop3"), flag("submission")); err != nil {
			t.Fatal(err)
		}
	}
	db.Close() //nolint:errcheck
	p, err := authsql.New(authsql.Config{Driver: "sqlite", DSN: dsn, SkipSchema: true,
		PasswordQuery: `SELECT password FROM users WHERE username = %u AND CASE %s
			WHEN 'imap' THEN imap WHEN 'pop3' THEN pop3 WHEN 'submission' THEN submission ELSE 1 END = 1`})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go protocol.NewServer([]protocol.Passdb{p}).Serve(ctx, ln) //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close(); p.Close() })      //nolint:errcheck
	return ln.Addr().String()
}

// One user, one rule: the protocol turned off answers as a wrong password, the
// other goes past the password check; then the reverse (#2227).
func TestAProtocolRuleRefusesOneProtocolAndKeepsAnother(t *testing.T) {
	plain := func(pw string) string { return base64.StdEncoding.EncodeToString([]byte("\x00alice\x00" + pw)) }
	logins := map[Protocol]func(pw string) []string{
		ProtocolIMAP:       func(pw string) []string { return []string{"a1 LOGIN alice " + pw} },
		ProtocolPOP3:       func(pw string) []string { return []string{"USER alice", "PASS " + pw} },
		ProtocolSubmission: func(pw string) []string { return []string{"EHLO client", "AUTH PLAIN " + plain(pw)} },
	}
	for _, r := range []struct {
		name string
		off  map[string]bool
		want map[Protocol]bool // refused
	}{
		{"imap off", map[string]bool{"imap": true}, map[Protocol]bool{ProtocolIMAP: true}},
		{"pop3 off", map[string]bool{"pop3": true}, map[Protocol]bool{ProtocolPOP3: true}},
		{"submission off", map[string]bool{"submission": true}, map[Protocol]bool{ProtocolSubmission: true}},
	} {
		t.Run(r.name, func(t *testing.T) {
			addr := ruleAuth(t, r.off)
			for proto, login := range logins {
				wrong := answer(t, proto, addr, login("wrong"))
				right := answer(t, proto, addr, login("secret"))
				if refused := right == wrong; refused != r.want[proto] {
					t.Errorf("%s: refused=%v, want %v (right %q, wrong %q)", proto, refused, r.want[proto], right, wrong)
				}
			}
		})
	}
}
