package dictsrv

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/proxy"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

// A dict behind internal mTLS is reached only by a client that speaks it; a
// plain dial would fail every operation on the stand (#1733).
func TestAProxiedDictSpeaksMTLSWhenTheServiceDoes(t *testing.T) {
	certFile, keyFile, caFile := writeInternalCerts(t)
	serverCfg, err := mtls.ServerConfig(certFile, keyFile, caFile)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	clientCfg, err := mtls.ClientConfig(certFile, keyFile, caFile, "yarilo-internal", 0, 0)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	real, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go New(map[string]dict.Dict{"metadata": real}, nil).Serve(ctx, ln) //nolint:errcheck

	set := &dict.OpSettings{Username: "u1@d.test"}
	tx, _ := real.Begin(ctx, set)
	_ = tx.Set("priv/one", []byte("first"))
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	secure := proxy.New(ln.Addr().String(), "metadata", clientCfg)
	t.Cleanup(func() { _ = secure.Close() })
	vals, found, err := secure.Lookup(ctx, set, "priv/one")
	if err != nil || !found {
		t.Fatalf("the mTLS client could not read: found=%v err=%v", found, err)
	}
	if string(vals[0]) != "first" {
		t.Errorf("read %q, want first", vals[0])
	}

	plain := proxy.New(ln.Addr().String(), "metadata", nil)
	t.Cleanup(func() { _ = plain.Close() })
	if _, _, err := plain.Lookup(ctx, set, "priv/one"); err == nil {
		t.Error("a plain client read a dict served behind mTLS")
	}
}

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
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caFile = filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caCert, _ := x509.ParseCertificate(caDER)
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
	certFile = filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(leafKey)
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, caFile
}

// TestWrapProxy_InternalMTLSHandshake guards #826: with the real
// mtls.ServerConfig (RequireAndVerifyClientCert) as PreambleTLS AND
// MaxLineLength set, a client's internal-mTLS handshake through the imap
// listener chain must COMPLETE. The regression was the maxLineLen wrapper
// buffering + line-scanning the binary ClientHello beneath TLS, so the server
// never sent a ServerHello. This exercises the real config pair the #824 unit
// test (simplified server cfg, no maxLineLen) missed.
