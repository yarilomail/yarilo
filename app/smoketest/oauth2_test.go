package main

import (
	"bufio"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOAuthBearerPayload(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(oauthBearer("a,b=c@d00001.test", "tok"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "n,a=a=2Cb=3Dc@d00001.test,\x01auth=Bearer tok\x01\x01"; string(raw) != want {
		t.Errorf("payload = %q, want %q", raw, want)
	}
}

// The token comes from the password grant over a CA the run names, as
// oauth2_tls_ca names it for yarilo-auth.
func TestOAuth2TokenThroughTheNamedCA(t *testing.T) {
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck
		got = []string{r.PostForm.Get("grant_type"), r.PostForm.Get("client_id"), r.PostForm.Get("username"), r.PostForm.Get("password")}
		if r.PostForm.Get("password") != "Yarilo!test1" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"tok","token_type":"Bearer"}`)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	setFlag(t, flagOAuth2TokenURL, srv.URL)
	setFlag(t, flagOAuth2ClientID, "yarilo-test")

	setFlag(t, flagOAuth2CA, "")
	if _, err := oauth2Token("u1@d00001.test", "Yarilo!test1"); err == nil {
		t.Fatal("a token came through a CA nobody named")
	}
	setFlag(t, flagOAuth2CA, ca)
	tok, err := oauth2Token("u1@d00001.test", "Yarilo!test1")
	if err != nil || tok != "tok" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if strings.Join(got, "|") != "password|yarilo-test|u1@d00001.test|Yarilo!test1" {
		t.Errorf("form = %v", got)
	}
	if _, err := oauth2Token("u1@d00001.test", "wrong"); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("wrong password: err = %v, want one naming invalid_grant", err)
	}
}

// Logged in by the token, the session must open as a mailbox, not stop at OK.
func TestOAuth2IMAPOpensTheInbox(t *testing.T) {
	c, srv := newFakeClient(t, 0)
	if err := oauth2IMAP(c, "u1@d00001.test", "tok"); err != nil {
		t.Fatal(err)
	}
	want := []string{"AUTHENTICATE OAUTHBEARER " + oauthBearer("u1@d00001.test", "tok"), `SELECT "INBOX"`}
	if got := srv.commands(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent %q, want %q", got, want)
	}
}

// lineFake answers each verb from replies and records the lines it was sent.
func lineFake(t *testing.T, replies map[string]string) (*pop3Client, func() []string) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	var mu sync.Mutex
	var sent []string
	go func() {
		rd := bufio.NewReader(server)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			mu.Lock()
			sent = append(sent, line)
			mu.Unlock()
			fmt.Fprintf(server, "%s\r\n", replies[strings.Fields(line)[0]])
		}
	}()
	return &pop3Client{conn: client, r: bufio.NewReader(client)}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

func TestOAuth2POP3ReadsTheMaildrop(t *testing.T) {
	p, sent := lineFake(t, map[string]string{"AUTH": "+OK", "STAT": "+OK 2 320"})
	if err := oauth2POP3(p, "u1@d00001.test", "tok"); err != nil {
		t.Fatal(err)
	}
	want := []string{"AUTH OAUTHBEARER " + oauthBearer("u1@d00001.test", "tok"), "STAT"}
	if got := sent(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent %q, want %q", got, want)
	}
	refused, _ := lineFake(t, map[string]string{"AUTH": "-ERR [AUTH] invalid token"})
	if err := oauth2POP3(refused, "u1@d00001.test", "tok"); err == nil {
		t.Error("a refused AUTH passed")
	}
}

// Without -oauth2-token-url the three rows are listed as skipped, by name, so
// every run shows the surface it did not check (#1311).
func TestTheOAuth2RowsAreAlwaysListed(t *testing.T) {
	names := []string{oauth2IMAPCheck, oauth2POP3Check, oauth2SubmissionCheck}
	for _, tc := range []struct {
		name, url string
		run       bool
	}{{"without the IdP", "", false}, {"with the IdP", "https://idp/token", true}} {
		t.Run(tc.name, func(t *testing.T) {
			setFlag(t, flagOAuth2TokenURL, tc.url)
			setFlag(t, flagIMAPUser, "u1@d00001.test")
			var got []string
			for _, c := range register() {
				if c.area != "oauth2" {
					continue
				}
				got = append(got, c.name)
				if (c.fn != nil) != tc.run || (c.skip == "") != tc.run {
					t.Errorf("%s: runs=%v skip=%q, want runs=%v", c.name, c.fn != nil, c.skip, tc.run)
				}
			}
			if strings.Join(got, "|") != strings.Join(names, "|") {
				t.Errorf("oauth2 rows = %q, want %q", got, names)
			}
		})
	}
}

// The bearer goes out as the AUTH line and the session then sends as the user.
func TestOAuth2SubmissionSendsAsTheUser(t *testing.T) {
	p, sent := lineFake(t, map[string]string{"AUTH": "235 2.7.0 ok", "MAIL": "250 ok", "RCPT": "250 ok", "RSET": "250 ok"})
	auth := "AUTH OAUTHBEARER " + oauthBearer("u1@d00001.test", "tok")
	if err := smtpSubmitTransaction(p.conn, "u1@d00001.test", auth); err != nil {
		t.Fatal(err)
	}
	if got := sent(); len(got) < 2 || got[0] != auth || got[1] != "MAIL FROM:<u1@d00001.test>" {
		t.Errorf("sent %q, want the bearer AUTH then MAIL FROM the user", got)
	}
}
