package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// The three OAuth2 rows; acceptance runs without the IdP and expects exactly
// these names skipped (hack/stand/accept.sh, #2231).
const (
	oauth2IMAPCheck       = "oauth2 imap AUTHENTICATE OAUTHBEARER (SASL-IR) + SELECT INBOX"
	oauth2POP3Check       = "oauth2 pop3 AUTH OAUTHBEARER + STAT"
	oauth2SubmissionCheck = "oauth2 submission AUTH OAUTHBEARER + MAIL FROM"
)

// oauth2Token gets an access token for user by the password grant.
func oauth2Token(user, pass string) (string, error) {
	hc := &http.Client{Timeout: *flagTimeout}
	if *flagOAuth2CA != "" {
		pem, err := os.ReadFile(*flagOAuth2CA)
		if err != nil {
			return "", fmt.Errorf("-oauth2-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return "", fmt.Errorf("-oauth2-ca: %s holds no PEM certificate", *flagOAuth2CA)
		}
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	form := url.Values{"grant_type": {"password"}, "client_id": {*flagOAuth2ClientID},
		"username": {user}, "password": {pass}, "scope": {"openid email"}}
	ctx, cancel := context.WithTimeout(context.Background(), *flagTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *flagOAuth2TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	var reply struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &reply); err != nil || resp.StatusCode != http.StatusOK || reply.AccessToken == "" {
		return "", fmt.Errorf("token endpoint: HTTP %d, error %q", resp.StatusCode, reply.Error)
	}
	return reply.AccessToken, nil
}

// oauthBearer is the base64 OAUTHBEARER client response (RFC 7628 §3.1).
func oauthBearer(user, token string) string {
	authzid := strings.NewReplacer("=", "=3D", ",", "=2C").Replace(user)
	return base64.StdEncoding.EncodeToString([]byte("n,a=" + authzid + ",\x01auth=Bearer " + token + "\x01\x01"))
}

func oauth2IMAP(c *imapClient, user, token string) error {
	if _, err := c.cmd("AUTHENTICATE OAUTHBEARER " + oauthBearer(user, token)); err != nil {
		return fmt.Errorf("AUTHENTICATE: %w", err)
	}
	if _, err := c.selectFolder("INBOX"); err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}
	return nil
}

func oauth2POP3(p *pop3Client, user, token string) error {
	if err := p.ok("AUTH OAUTHBEARER " + oauthBearer(user, token)); err != nil {
		return err
	}
	_, _, err := p.stat()
	return err
}

func checkOAuth2IMAP() error {
	user := *flagIMAPUser
	token, err := oauth2Token(user, *flagIMAPPass)
	if err != nil {
		return err
	}
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()
	return oauth2IMAP(c, user, token)
}

func checkOAuth2POP3() error {
	user := *flagIMAPUser
	token, err := oauth2Token(user, *flagIMAPPass)
	if err != nil {
		return err
	}
	p, err := pop3Open()
	if err != nil {
		return err
	}
	defer p.close()
	return oauth2POP3(p.pop3Client, user, token)
}

func checkOAuth2Submission() error {
	user := *flagIMAPUser
	token, err := oauth2Token(user, *flagIMAPPass)
	if err != nil {
		return err
	}
	conn, _, err := submissionTLS()
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := smtpSubmitTransaction(conn, user, "AUTH OAUTHBEARER "+oauthBearer(user, token)); err != nil {
		return err
	}
	smtpQuit(conn)
	return nil
}
