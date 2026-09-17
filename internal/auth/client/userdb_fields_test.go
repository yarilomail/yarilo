package client

import (
	"bufio"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

var errNoVerdict = errors.New("the exchange ended without a verdict")

// A relayed login must reach the session with everything the service answered:
// this client once kept five fields, so a user whose userdb named its own mail
// location or ACL identity was logged in without them (#1890).
//
// The values are deliberately unlike any global default -- a row on defaults
// would pass with the fields dropped.
func TestARelayedLoginCarriesTheUserdbAnswer(t *testing.T) {
	answer := "OK\t1\tuser=u1@d00001.test\ttoken=tok" +
		"\tuserdb_home=/srv/h/u1\tuserdb_mail=mdbox:/srv/m/u1\tuserdb_mailbox_format=mdbox" +
		"\tuserdb_groups=staff,ops\tuserdb_acl_user=shared@d00001.test\tuserdb_acl_groups=readers" +
		"\tuserdb_quota_rule=*:storage=5G\tuserdb_quota_over_flag=over" +
		"\tuserdb_volatile_dir=/srv/v/u1\tuserdb_index_dir=/srv/i/u1\tuserdb_control_dir=/srv/c/u1" +
		"\tuserdb_alt_dir=/srv/alt/u1\tuserdb_mail_path=/srv/m/u1\tuserdb_inbox_path=/srv/m/u1/INBOX" +
		"\tuserdb_director_tag=tag-b"

	want := &protocol.AuthResponse{
		Result: protocol.AuthOK, Username: "u1@d00001.test",
		Home: "/srv/h/u1", MailLoc: "mdbox:/srv/m/u1", MailboxFormat: "mdbox",
		Groups: []string{"staff", "ops"}, ACLUser: "shared@d00001.test", ACLGroups: []string{"readers"},
		QuotaRules: []string{"*:storage=5G"}, QuotaOverFlag: "over", DirectorTag: "tag-b",
		VolatileDir: "/srv/v/u1", IndexDir: "/srv/i/u1", ControlDir: "/srv/c/u1", AltDir: "/srv/alt/u1",
		MailPath: "/srv/m/u1", InboxPath: "/srv/m/u1/INBOX",
	}

	for _, verb := range []string{"AUTH", "CONT"} {
		t.Run(verb, func(t *testing.T) {
			c, err := New(answerOnce(t, verb, answer), nil, Options{})
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			defer c.Close() //nolint:errcheck

			res, err := relayedLogin(c, verb)
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			if got := res.Response(); !reflect.DeepEqual(got, want) {
				t.Errorf("the session would see\n  %+v\nwant\n  %+v", got, want)
			}
			if res.DirectorTag != "tag-b" {
				t.Errorf("director tag %q, want tag-b", res.DirectorTag)
			}
		})
	}
}

// relayedLogin drives the password half or the SASL half: both end in the same
// OK answer, and both feed the same session.
func relayedLogin(c *Client, verb string) (*AuthResult, error) {
	if verb == "AUTH" {
		return c.Authenticate("u1@d00001.test", "secret", "imap", "10.0.0.1", "s1")
	}
	ex, step, err := c.BeginSASL("SCRAM-SHA-256", "imap", "10.0.0.1", "s1", nil, []byte("n,,n=u1,r=nonce"))
	if err != nil {
		return nil, err
	}
	if step.Done {
		return step.Result, nil
	}
	step, err = ex.Next([]byte("c=biws,r=nonce,p=proof"))
	if err != nil {
		return nil, err
	}
	if !step.Done || step.Result == nil {
		return nil, errNoVerdict
	}
	return step.Result, nil
}

// answerOnce replies to the first request of the named verb with line, the id
// substituted, so one fake serves both the password and the SASL answer.
func answerOnce(t *testing.T, verb, line string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		r := bufio.NewReader(conn)
		_, _ = conn.Write([]byte("VERSION\t1\t0\nMECH\tPLAIN\tactive\nDONE\n"))
		for {
			req, rerr := r.ReadString('\n')
			if rerr != nil {
				return
			}
			f := strings.Split(strings.TrimRight(req, "\r\n"), "\t")
			if f[0] == "AUTH" && verb == "CONT" {
				_, _ = conn.Write([]byte("CONT\t" + f[1] + "\tcj1ub25jZQ==\n"))
				continue
			}
			if f[0] != verb {
				continue
			}
			parts := strings.SplitN(line, "\t", 3)
			_, _ = conn.Write([]byte(parts[0] + "\t" + f[1] + "\t" + parts[2] + "\n"))
		}
	}()
	return ln.Addr().String()
}
