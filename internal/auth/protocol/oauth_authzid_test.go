package protocol

import (
	"fmt"
	"testing"
)

// claimPassdb answers as the OAuth2 passdb does: the token names alice, and an
// authzid, when the client sends one, must be that name.
type claimPassdb struct{ tokenPassdb }

func (claimPassdb) Authenticate(req *Request) (Result, error) {
	if req.Password != "good-token" || (req.Username != "" && req.Username != "alice") {
		return ResultNext, nil
	}
	req.Fields.Set("user", "alice")
	return ResultOK, nil
}

// The authzid is optional (RFC 7628 §3.1): without one the token's user logs in.
func TestOAuthBearerWithoutAnAuthzid(t *testing.T) {
	conn, rd, _ := oauthServer(t, claimPassdb{})
	for i, tc := range []struct {
		name, msg string
		ok        bool
	}{
		{"no authzid", "n,,\x01auth=Bearer good-token\x01\x01", true},
		{"authzid naming the token's user", "n,a=alice,\x01auth=Bearer good-token\x01\x01", true},
		{"authzid naming someone else", "n,a=bob,\x01auth=Bearer good-token\x01\x01", false},
		{"empty authzid", "n,a=,\x01auth=Bearer good-token\x01\x01", false},
		{"no authzid, bad token", "n,,\x01auth=Bearer bad-token\x01\x01", false},
	} {
		id := fmt.Sprint(i + 1)
		fmt.Fprintf(conn, "AUTH\t%s\t%s\tservice=jmap\tresp=%s\n", id, MechOAuthBearer, b64(tc.msg))
		f := reply(t, rd)
		if got := f[0] == "OK"; got != tc.ok {
			t.Errorf("%s: %q, want OK %v", tc.name, f, tc.ok)
		}
		if tc.ok && !containsField(f, "user=alice") {
			t.Errorf("%s: %q, want user=alice", tc.name, f)
		}
		if f[0] == "CONT" {
			fmt.Fprintf(conn, "CONT\t%s\t%s\n", id, b64("\x01"))
			reply(t, rd)
		}
	}
}

// What follows the chain is keyed by the user it resolved: the policy's
// after-check of a login that named no one hears the token's user.
func TestAnOAuthLoginWithoutAnAuthzidReachesThePolicyByName(t *testing.T) {
	p := newStubPolicy(true)
	srv := NewServer([]Passdb{claimPassdb{}}, WithPolicy(p, PolicyMode{CheckBefore: true, CheckAfter: true}))
	addr, cancel := startTestServer(t, srv)
	defer cancel()
	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()
	fmt.Fprintf(conn, "AUTH\t1\t%s\tservice=jmap\tresp=%s\n", MechOAuthBearer, b64("n,,\x01auth=Bearer good-token\x01\x01"))
	if !sc.Scan() {
		t.Fatal("no reply")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.afterCalls) != 1 || p.afterCalls[0].req.Username != "alice" {
		t.Errorf("CheckAfter heard %+v, want the token's user alice", p.afterCalls)
	}
}

func containsField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}
