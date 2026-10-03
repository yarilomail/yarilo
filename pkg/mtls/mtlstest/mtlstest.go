// Package mtlstest issues an internal CA and role certificates for tests.
package mtlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mtls"
)

// ServerName is the pinned internal name every issued certificate carries.
const ServerName = "yarilo-internal"

// CA is a throwaway internal CA writing its files under a test's temp dir.
type CA struct {
	CAFile string
	dir    string
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	serial atomic.Int64
}

// NewCA creates the CA and writes its certificate.
func NewCA(t testing.TB) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "yarilo-internal-ca"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1<<31, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	ca := &CA{dir: t.TempDir(), cert: cert, key: key}
	ca.serial.Store(1)
	ca.CAFile = ca.write(t, "ca.crt", "CERTIFICATE", der)
	return ca
}

// Role issues a certificate carrying role r.
func (ca *CA) Role(t testing.TB, r mtls.Role) (certFile, keyFile string) {
	t.Helper()
	return ca.Issue(t, string(r), string(r)+mtls.RoleSuffix)
}

// Issue issues a leaf for both ends with ServerName plus dnsNames.
func (ca *CA) Issue(t testing.TB, name string, dnsNames ...string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	n := ca.serial.Add(1)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(n),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     append([]string{ServerName}, dnsNames...),
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("%s-%d", name, n)
	return ca.write(t, base+".crt", "CERTIFICATE", der), ca.write(t, base+".key", "EC PRIVATE KEY", kder)
}

func (ca *CA) write(t testing.TB, name, typ string, der []byte) string {
	t.Helper()
	p := filepath.Join(ca.dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
