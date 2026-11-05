package sieve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

type nopMTA struct{}

func (nopMTA) NewSession(*goSmtp.Conn) (goSmtp.Session, error) { return nopSession{}, nil }

type nopSession struct{}

func (nopSession) Mail(string, *goSmtp.MailOptions) error { return nil }
func (nopSession) Rcpt(string, *goSmtp.RcptOptions) error { return nil }
func (nopSession) Data(r io.Reader) error                 { _, err := io.Copy(io.Discard, r); return err }
func (nopSession) Reset()                                 {}
func (nopSession) Logout() error                          { return nil }

// mtaCA issues a leaf for names; it returns the CA as PEM.
func mtaCA(t *testing.T, ips []net.IP, names []string) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test internal CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "mta"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: ips, DNSNames: names, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startMTA serves STARTTLS, or implicit TLS for smtps, with leaf.
func startMTA(t *testing.T, leaf tls.Certificate, implicit bool) string {
	t.Helper()
	tc := &tls.Config{Certificates: []tls.Certificate{leaf}}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if implicit {
		ln = tls.NewListener(ln, tc)
	}
	srv := goSmtp.NewServer(nopMTA{})
	srv.Domain = "mta.test"
	srv.TLSConfig = tc
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func TestSieveMTAIsVerifiedAgainstTheNamedCA(t *testing.T) {
	loopback := []net.IP{net.ParseIP("127.0.0.1")}
	cases := []struct {
		name    string
		ssl     string
		ips     []net.IP
		names   []string
		withCA  bool
		verify  bool
		wantErr string
	}{
		{"starttls, the MTA's CA is named", "starttls", loopback, nil, true, true, ""},
		{"starttls, no CA named: system roots refuse it", "starttls", loopback, nil, false, true, "certificate"},
		{"starttls, a certificate for another name", "starttls", nil, []string{"other.test"}, true, true, "certificate"},
		{"starttls, ssl_verify off keeps its meaning", "starttls", nil, []string{"other.test"}, true, false, ""},
		{"smtps, the MTA's CA is named", "smtps", loopback, nil, true, true, ""},
		{"smtps, no CA named: system roots refuse it", "smtps", loopback, nil, false, true, "certificate"},
		{"submissions, a certificate for another name", "submissions", nil, []string{"other.test"}, true, true, "certificate"},
		{"smtps, ssl_verify off keeps its meaning", "smtps", nil, []string{"other.test"}, true, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caPEM, leaf := mtaCA(t, c.ips, c.names)
			cfg := config.SieveConfig{SubmissionHost: startMTA(t, leaf, c.ssl != "starttls"), SubmissionSSL: c.ssl,
				SubmissionSSLVerify: c.verify, SubmissionTimeout: 5}
			if c.withCA {
				cfg.SubmissionSSLCA = filepath.Join(t.TempDir(), "ca.crt")
				if err := os.WriteFile(cfg.SubmissionSSLCA, caPEM, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cl, err := newSender(cfg).dial(t.Context())
			if err == nil {
				err = cl.Hello("localhost")
				cl.Close()
			}
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("dial: %v, want a verified session", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("dial: %v, want a refusal naming the %s", err, c.wantErr)
			}
		})
	}
}

// A CA file with no certificate, or none at all, is an error, not the system roots.
func TestASieveMTACAThatCannotBeReadIsRefused(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(empty, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{empty, "holds no certificate"},
		{filepath.Join(dir, "missing.crt"), "no such file"},
	} {
		_, err := newSender(config.SieveConfig{SubmissionSSLCA: c.path, SubmissionSSLVerify: true}).tlsConfig("127.0.0.1")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("tlsConfig(%s) = %v, want %q", filepath.Base(c.path), err, c.want)
		}
	}
}
