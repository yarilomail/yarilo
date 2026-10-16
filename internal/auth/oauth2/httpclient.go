package oauth2

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// NewHTTPClient is the client one entry's calls go through. A non-empty
// caFile replaces the system roots with the CAs it holds.
func NewHTTPClient(timeout time.Duration, caFile string) (*http.Client, error) {
	if caFile == "" {
		return &http.Client{Timeout: timeout}, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("oauth2/tls_ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("oauth2/tls_ca: %s holds no PEM certificate", caFile)
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("oauth2/tls_ca: default transport is %T", http.DefaultTransport)
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Timeout: timeout, Transport: tr}, nil
}
