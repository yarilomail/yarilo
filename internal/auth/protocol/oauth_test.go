package protocol

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// tokenPassdb accepts one bearer token for alice, the way the validator does.
type tokenPassdb struct{ scope, openid string }

func (tokenPassdb) Authenticate(req *Request) (Result, error) {
	if req.Username != "alice" || req.Password != "good-token" {
		return ResultNext, nil
	}
	req.Fields.Set("user", "alice")
	return ResultOK, nil
}

func (p tokenPassdb) OAuth2Failure() (string, string) { return p.scope, p.openid }

// oauthServer serves chain and returns a handshaken connection plus the
// handshake's MECH names.
func oauthServer(t *testing.T, chain ...Passdb) (net.Conn, *bufio.Reader, []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go NewServer(chain).Serve(ctx, ln) //nolint:errcheck
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	fmt.Fprintf(conn, "VERSION\t1\t0\n")
	rd := bufio.NewReader(conn)
	var mechs []string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if m, ok := strings.CutPrefix(line, "MECH\t"); ok {
			mechs = append(mechs, strings.SplitN(m, "\t", 2)[0])
		}
		if line == "DONE" {
			return conn, rd, mechs
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func reply(t *testing.T, rd *bufio.Reader) []string {
	t.Helper()
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.Split(strings.TrimRight(line, "\n"), "\t")
}

func TestOAuthIsAnnouncedOnlyWithATokenValidator(t *testing.T) {
	_, _, with := oauthServer(t, tokenPassdb{})
	_, _, without := oauthServer(t, &credPassdb{"alice", "pw"})
	has := func(ms []string, m string) bool {
		for _, x := range ms {
			if x == m {
				return true
			}
		}
		return false
	}
	for _, m := range []string{MechOAuthBearer, MechXOAuth2} {
		if !has(with, m) {
			t.Errorf("%s not announced with a token validator (%v)", m, with)
		}
		if has(without, m) {
			t.Errorf("%s announced without a token validator (%v)", m, without)
		}
	}
}

const (
	bearerMsg = "n,a=alice,\x01auth=Bearer %s\x01\x01"
	xoauthMsg = "user=alice\x01auth=Bearer %s\x01\x01"
)

func TestAnOAuthLoginRunsTheChain(t *testing.T) {
	conn, rd, _ := oauthServer(t, tokenPassdb{})
	for i, tc := range []struct{ mech, msg string }{
		{MechOAuthBearer, bearerMsg},
		{MechXOAuth2, xoauthMsg},
	} {
		id := fmt.Sprint(i + 1)
		fmt.Fprintf(conn, "AUTH\t%s\t%s\tservice=imap\tresp=%s\n", id, tc.mech, b64(fmt.Sprintf(tc.msg, "good-token")))
		if f := reply(t, rd); f[0] != "OK" || !strings.Contains(strings.Join(f, "\t"), "user=alice") {
			t.Errorf("%s: %q, want OK for alice", tc.mech, f)
		}
	}
}

// Without an initial response the client is asked for its message first.
func TestAnOAuthLoginWithoutAnInitialResponse(t *testing.T) {
	conn, rd, _ := oauthServer(t, tokenPassdb{})
	fmt.Fprintf(conn, "AUTH\t1\t%s\tservice=imap\n", MechOAuthBearer)
	if f := reply(t, rd); f[0] != "CONT" {
		t.Fatalf("%q, want an empty challenge", f)
	}
	fmt.Fprintf(conn, "CONT\t1\t%s\n", b64(fmt.Sprintf(bearerMsg, "good-token")))
	if f := reply(t, rd); f[0] != "OK" {
		t.Fatalf("%q, want OK", f)
	}
}

// A refusal is a challenge carrying the status; the login fails on the
// client's acknowledgement, as RFC 7628 §3.2.2 has it.
func TestARefusedOAuthLoginExplainsItself(t *testing.T) {
	db := tokenPassdb{scope: "mail profile", openid: "https://idp.example/.well-known/openid-configuration"}
	for _, tc := range []struct {
		name, mech, msg string
		want            map[string]string
	}{
		{"bad token", MechOAuthBearer, fmt.Sprintf(bearerMsg, "bad-token"),
			map[string]string{"status": "invalid_token", "scope": "mail profile", "openid-configuration": db.openid}},
		{"not a bearer", MechOAuthBearer, "n,a=alice,\x01auth=Basic x\x01\x01",
			map[string]string{"status": "invalid_token"}},
		{"malformed", MechOAuthBearer, "garbage",
			map[string]string{"status": "invalid_request"}},
		{"bad token", MechXOAuth2, fmt.Sprintf(xoauthMsg, "bad-token"),
			map[string]string{"status": "401", "schemes": "bearer", "scope": "mail profile"}},
		{"no user", MechXOAuth2, "auth=Bearer good-token\x01\x01",
			map[string]string{"status": "400", "schemes": "bearer"}},
	} {
		t.Run(tc.mech+" "+tc.name, func(t *testing.T) {
			conn, rd, _ := oauthServer(t, db)
			fmt.Fprintf(conn, "AUTH\t7\t%s\tservice=imap\tresp=%s\n", tc.mech, b64(tc.msg))
			f := reply(t, rd)
			if f[0] != "CONT" || len(f) < 3 {
				t.Fatalf("%q, want a challenge carrying the refusal", f)
			}
			raw, _ := base64.StdEncoding.DecodeString(f[2])
			var got map[string]string
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("challenge %q is not JSON: %v", raw, err)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q (%s)", k, got[k], v, raw)
				}
			}
			fmt.Fprintf(conn, "CONT\t7\t%s\n", b64("\x01"))
			if f := reply(t, rd); f[0] != "FAIL" {
				t.Fatalf("after the acknowledgement %q, want FAIL", f)
			}
		})
	}
}

func TestOAuthWithoutATokenValidatorIsRefused(t *testing.T) {
	conn, rd, _ := oauthServer(t, &credPassdb{"alice", "pw"})
	fmt.Fprintf(conn, "AUTH\t1\t%s\tservice=imap\tresp=%s\n", MechXOAuth2, b64(fmt.Sprintf(xoauthMsg, "good-token")))
	if f := reply(t, rd); f[0] != "FAIL" {
		t.Fatalf("%q, want FAIL", f)
	}
}
