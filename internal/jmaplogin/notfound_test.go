package jmaplogin

import (
	"context"
	"net/http"
	"testing"
)

// A path the backend does not serve is 404 before any credential is asked,
// with the CORS headers a browser needs to read it; a served path still asks.
func TestAPathNothingServesIsNotFoundBeforeAuthentication(t *testing.T) {
	const origin = "http://localhost:3001"
	base := startProxy(t, Options{CORSAllowOrigins: []string{origin}})
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/.well-known/webfinger?resource=https://mail.example.com&rel=http://openid.net/specs/connect/1.0/issuer", http.StatusNotFound},
		{"/.well-known/openid-configuration", http.StatusNotFound},
		{"/", http.StatusNotFound},
		{"/jmapx/api/", http.StatusNotFound},
		{"/.well-known/jmap", http.StatusUnauthorized},
		{"/jmap/api/", http.StatusUnauthorized},
		{"/jmap/eventsource/?types=*", http.StatusUnauthorized},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Origin", origin)
			resp, err := keepAliveClient().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close() //nolint:errcheck
			if resp.StatusCode != tc.want {
				t.Errorf("status %d, want %d", resp.StatusCode, tc.want)
			}
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("allow-origin %q, want %q", got, origin)
			}
			if tc.want == http.StatusNotFound && resp.Header.Get("WWW-Authenticate") != "" {
				t.Error("a 404 asked for credentials")
			}
		})
	}
}
