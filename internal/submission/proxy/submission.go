// Package proxy implements the outbound submission proxy.
// Accepts an authenticated session from an MUA and forwards it to
// the configured upstream MTA — one TCP connection per message, fail-closed.
package proxy

import (
	"cmp"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/pkg/config"
)

// Submission proxies outbound mail to the configured upstream MTA.
// One connection per message, fail-closed (4xx to client) on any transport error.
type Submission struct {
	cfg      config.RelayConfig
	hostname string // EHLO hostname sent to the upstream
}

func New(cfg config.RelayConfig, hostname string) *Submission {
	if hostname == "" {
		hostname = "localhost"
	}
	return &Submission{cfg: cfg, hostname: hostname}
}

// capabilities holds ESMTP extensions advertised by the upstream MTA.
type capabilities struct {
	has8BitMIME bool
	hasDSN      bool
	hasXCLIENT  bool
}

// Send forwards a message to the upstream MTA. clientIP feeds XCLIENT when
// Trusted; login is the session's user, which %u in the relay user names; sid
// names the session in the log. Transport errors → 451 4.4.0, relay auth
// failures → 451 4.7.0. SMTP-level rejections are passed through as-is.
func (s *Submission) Send(from string, rcpts []string, body io.Reader, clientIP net.IP, login, sid string) error {
	if s.cfg.Host == "" {
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 3, 0},
			Message:      "Upstream MTA not configured",
		}
	}
	if login == "" && s.needsLogin() {
		slog.Error("submission/proxy: the relay names the sender, and the session has no user", "sid", sid)
		return errRelayAuth
	}

	c, err := s.dial()
	if err != nil {
		slog.Error("submission/proxy: connect failed", "host", s.cfg.Host, "err", err)
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 4, 0},
			Message:      "Failed to connect to upstream MTA",
		}
	}
	defer c.Close()

	if err := c.Hello(s.hostname); err != nil {
		slog.Error("submission/proxy: EHLO failed", "err", err)
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 4, 0},
			Message:      "Upstream EHLO failed",
		}
	}

	caps := s.probeCapabilities(c)
	slog.Debug("submission/proxy: upstream capabilities",
		"host", s.cfg.Host,
		"8bitmime", caps.has8BitMIME,
		"dsn", caps.hasDSN,
		"xclient", caps.hasXCLIENT,
	)

	if s.cfg.Trusted && caps.hasXCLIENT && clientIP != nil {
		if err := c.XClient(goSmtp.XClientData{Addr: clientIP}); err != nil {
			slog.Warn("submission/proxy: XCLIENT failed", "err", err)
		}
	}

	if authcid, authzid := s.relayAuth(login); authcid != "" {
		auth := sasl.NewPlainClient(authzid, authcid, s.cfg.Password)
		if err := c.Auth(auth); err != nil {
			slog.Error("submission/proxy: relay auth failed", "sid", sid, "user", authcid, "for", authzid, "err", err)
			return errRelayAuth
		}
	}

	if err := c.SendMail(from, rcpts, body); err != nil {
		var smtpErr *goSmtp.SMTPError
		if errors.As(err, &smtpErr) {
			return smtpErr
		}
		slog.Error("submission/proxy: send failed", "err", err)
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 4, 0},
			Message:      "Upstream send error",
		}
	}
	return nil
}

func (s *Submission) probeCapabilities(c *goSmtp.Client) capabilities {
	has8bit, _ := c.Extension("8BITMIME")
	hasDSN, _ := c.Extension("DSN")
	hasXCLIENT, _ := c.Extension("XCLIENT")
	return capabilities{
		has8BitMIME: has8bit,
		hasDSN:      hasDSN,
		hasXCLIENT:  hasXCLIENT,
	}
}

func (s *Submission) dial() (*goSmtp.Client, error) {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	ct := s.connectTimeout()

	var c *goSmtp.Client

	switch s.cfg.SSL {
	case "smtps":
		tc, err := s.tlsConfig()
		if err != nil {
			return nil, err
		}
		d := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: ct},
			Config:    tc,
		}
		conn, err := d.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		c = goSmtp.NewClient(conn)
	case "starttls":
		tc, err := s.tlsConfig()
		if err != nil {
			return nil, err
		}
		conn, err := net.DialTimeout("tcp", addr, ct)
		if err != nil {
			return nil, err
		}
		var startErr error
		c, startErr = goSmtp.NewClientStartTLS(conn, tc)
		if startErr != nil {
			conn.Close()
			return nil, startErr
		}
	default: // "no"
		conn, err := net.DialTimeout("tcp", addr, ct)
		if err != nil {
			return nil, err
		}
		c = goSmtp.NewClient(conn)
	}

	cmd := s.commandTimeout()
	c.CommandTimeout = cmd
	c.SubmissionTimeout = cmd + 2*time.Minute
	return c, nil
}

// tlsConfig verifies the relay against submission_relay_ssl_ca when set, the
// system roots otherwise.
func (s *Submission) tlsConfig() (*tls.Config, error) {
	tc := &tls.Config{
		ServerName:         s.cfg.Host,
		InsecureSkipVerify: !s.cfg.SSLVerify, //nolint:gosec
	}
	if s.cfg.SSLCA == "" {
		return tc, nil
	}
	pem, err := os.ReadFile(s.cfg.SSLCA)
	if err != nil {
		return nil, fmt.Errorf("submission/proxy: relay CA: %w", err)
	}
	tc.RootCAs = x509.NewCertPool()
	if !tc.RootCAs.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("submission/proxy: relay CA %s holds no certificate", s.cfg.SSLCA)
	}
	return tc, nil
}

func (s *Submission) connectTimeout() time.Duration {
	return time.Duration(s.cfg.ConnectTimeout) * time.Second
}

func (s *Submission) commandTimeout() time.Duration {
	return time.Duration(s.cfg.CommandTimeout) * time.Second
}

// relayAuth is the PLAIN identity the relay is given: the relay user, or with a
// master user that master acting for the relay user, as the reference does.
func (s *Submission) relayAuth(login string) (authcid, authzid string) {
	user := strings.NewReplacer("%{user}", login, "%u", login).Replace(s.cfg.User)
	if s.cfg.MasterUser != "" {
		return s.cfg.MasterUser, cmp.Or(user, login)
	}
	return user, ""
}

// needsLogin: a relay user built from %u, or a master acting for nobody named,
// cannot be given without the session's user.
func (s *Submission) needsLogin() bool {
	return (s.cfg.MasterUser != "" && s.cfg.User == "") ||
		strings.Contains(s.cfg.User, "%u") || strings.Contains(s.cfg.User, "%{user}")
}

// errRelayAuth is what the client is told when the relay cannot be logged in
// to; the reason goes to the log with the session id.
var errRelayAuth = &goSmtp.SMTPError{
	Code:         451,
	EnhancedCode: goSmtp.EnhancedCode{4, 7, 0},
	Message:      "Temporary authentication failure, try again later",
}
