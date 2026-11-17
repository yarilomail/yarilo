package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseMinTLS(t *testing.T) {
	cases := []struct {
		in   string
		want uint16
		ok   bool
	}{
		{"", tls.VersionTLS12, true}, {"TLS1.2", tls.VersionTLS12, true}, {"TLSv1.2", tls.VersionTLS12, true},
		{"TLS1.3", tls.VersionTLS13, true}, {"TLSv1.3", tls.VersionTLS13, true}, {"tlsv1.3", tls.VersionTLS13, true},
		{"TLS1.3x", 0, false}, {"TLSv1.1", 0, false}, {"TLSv1", 0, false}, {"SSLv2", 0, false}, {"ANY", 0, false}, {"1.3", 0, false},
	}
	for _, c := range cases {
		got, err := ParseMinTLS(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%q: %d %v, want %d ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func loadMinTLS(t *testing.T, body string) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	return err
}

// Every place ssl_min_protocol can be written refuses a typo at load, naming
// the key; before #2568 each one was read as TLS1.2.
func TestMinTLSRefusedEverywhere(t *testing.T) {
	if err := loadMinTLS(t, "general:\n  ssl:\n    ssl_min_protocol: TLS1.3x\n"); err == nil || !strings.Contains(err.Error(), "general.ssl.ssl_min_protocol") {
		t.Errorf("general: %v", err)
	}
	if err := loadMinTLS(t, "general:\n  ssl:\n    tls_min_version: TLSv1.4\n"); err == nil || !strings.Contains(err.Error(), "ssl_min_protocol") {
		t.Errorf("the alias: %v", err)
	}
	st := reflect.TypeOf(ServicesConfig{})
	for i := range st.NumField() {
		name, _, _ := strings.Cut(st.Field(i).Tag.Get("koanf"), ",")
		if st.Field(i).Type != reflect.TypeOf(&ServiceConfig{}) {
			continue
		}
		body := "services:\n  " + name + ":\n    ssl:\n      ssl_min_protocol: TLS1.3x\n"
		if err := loadMinTLS(t, body); err == nil || !strings.Contains(err.Error(), "services."+name+".ssl.ssl_min_protocol") {
			t.Errorf("services.%s: %v", name, err)
		}
		ok := "services:\n  " + name + ":\n    ssl:\n      ssl_min_protocol: TLSv1.3\n"
		if err := loadMinTLS(t, ok); err != nil {
			t.Errorf("services.%s TLSv1.3 refused: %v", name, err)
		}
	}
}

// The listener gets the floor asked for: TLSv1.3 was a TLS1.2 floor before.
func TestBuildTLSConfigFloor(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	dir := t.TempDir()
	cert, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	_ = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	for in, want := range map[string]uint16{"TLSv1.3": tls.VersionTLS13, "TLS1.3": tls.VersionTLS13, "TLS1.2": tls.VersionTLS12, "": tls.VersionTLS12} {
		c, err := BuildTLSConfig(SSLConfig{SSLServerCert: cert, SSLServerKey: keyFile, SSLMinProtocol: in})
		if err != nil || c.MinVersion != want {
			t.Errorf("%q: %v, MinVersion %x want %x", in, err, c.MinVersion, want)
		}
	}
	if _, err := BuildTLSConfig(SSLConfig{SSLServerCert: cert, SSLServerKey: keyFile, SSLMinProtocol: "TLS1.3x"}); err == nil {
		t.Error("a typo built a listener")
	}
}
