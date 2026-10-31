package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// testCA issues a leaf for the given IPs and names; it returns the CA as PEM.
func testCA(t *testing.T, ips []net.IP, names []string) (caPEM []byte, leaf tls.Certificate) {
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
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "relay"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: ips, DNSNames: names, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startTLSRelay serves STARTTLS with leaf and returns its host and port.
func startTLSRelay(t *testing.T, leaf tls.Certificate) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := goSmtp.NewServer(authRelay{&relaySeen{}})
	srv.Domain = "relay.test"
	srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{leaf}}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return host, p
}

func TestRelayIsVerifiedAgainstTheNamedCA(t *testing.T) {
	loopback := []net.IP{net.ParseIP("127.0.0.1")}
	cases := []struct {
		name    string
		ips     []net.IP
		names   []string
		withCA  bool
		verify  bool
		wantErr string
	}{
		{"the relay's CA is named", loopback, nil, true, true, ""},
		{"no CA named: system roots refuse it", loopback, nil, false, true, "certificate"},
		{"a certificate for another name", nil, []string{"other.test"}, true, true, "certificate"},
		{"ssl_verify off keeps its meaning", nil, []string{"other.test"}, true, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caPEM, leaf := testCA(t, c.ips, c.names)
			host, port := startTLSRelay(t, leaf)
			cfg := config.RelayConfig{Host: host, Port: port, SSL: "starttls", SSLVerify: c.verify,
				ConnectTimeout: 5, CommandTimeout: 5}
			if c.withCA {
				cfg.SSLCA = filepath.Join(t.TempDir(), "ca.crt")
				if err := os.WriteFile(cfg.SSLCA, caPEM, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The handshake runs on the first exchange after STARTTLS: Send's Hello.
			c2, err := New(cfg, "mx.test").dial()
			if err == nil {
				err = c2.Hello("mx.test")
				c2.Close()
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

// A CA file with no certificate is an error, not the system roots.
func TestARelayCAWithNoCertificateIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(config.RelayConfig{Host: "127.0.0.1", SSLCA: path, SSLVerify: true}, "h").tlsConfig()
	if err == nil || !strings.Contains(err.Error(), "holds no certificate") {
		t.Errorf("tlsConfig = %v, want the empty CA named", err)
	}
}
