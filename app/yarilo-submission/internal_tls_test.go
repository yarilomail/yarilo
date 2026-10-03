package main

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
	"testing"
	"time"

	submsvr "github.com/yarilomail/yarilo/internal/submission"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

// writeInternalCerts writes a CA and one leaf for both ends, as the shared
// internal-tls secret is.
func writeInternalCerts(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "yarilo-internal-ca"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1<<31, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "yarilo-internal"},
		DNSNames:     []string{"yarilo-internal"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	kder, _ := x509.MarshalECPrivateKey(leafKey)
	write := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return write("tls.crt", "CERTIFICATE", leafDER), write("tls.key", "EC PRIVATE KEY", kder), write("ca.crt", "CERTIFICATE", caDER)
}

// The login pod dials this backend with internal mTLS; the handshake must
// complete on the listener main builds (#2133).
func TestSubmissionBackendTerminatesInternalTLS(t *testing.T) {
	certFile, keyFile, caFile := writeInternalCerts(t)
	cfg := &config.Config{}
	cfg.InternalTLS.Enabled = true
	cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA = certFile, keyFile, caFile

	srv, err := newServer(cfg, submsvr.Options{AuthAddr: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln, nil) }()

	clientCfg, err := mtls.ClientConfig(certFile, keyFile, caFile, "yarilo-internal", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("internal mTLS handshake with the submission backend: %v", err)
	}
	conn.Close()
}
