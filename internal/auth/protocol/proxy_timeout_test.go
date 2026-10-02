package protocol

import "testing"

func TestExtractProxyTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]string
		want   int
	}{
		{"absent", nil, 0},
		{"passdb column", map[string]string{"proxy_timeout": "7"}, 7},
		{"userdb-scoped wins", map[string]string{"proxy_timeout": "7", "userdb_proxy_timeout": "9"}, 9},
		{"negative", map[string]string{"proxy_timeout": "-1"}, 0},
		{"not a number", map[string]string{"proxy_timeout": "30s"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFields()
			for k, v := range tc.fields {
				f.Set(k, v)
			}
			if got := extractProxyTimeout(f); got != tc.want {
				t.Fatalf("extractProxyTimeout = %d, want %d", got, tc.want)
			}
		})
	}
}

// A passdb column reaches the login proxy's side of the wire.
func TestProxyTimeoutCrossesTheWire(t *testing.T) {
	f := NewFields()
	f.Set("user", "u@x")
	f.Set("proxy_timeout", "7")
	reply := buildAuthOK("id1", &AuthResponse{Username: "u@x", Fields: f})
	if !containsToken(reply, "proxy_timeout=7") {
		t.Fatalf("reply %q missing proxy_timeout", reply)
	}
	res := &AuthResponse{}
	if !ApplyAuthOKToken(res, "proxy_timeout=7") || res.ProxyTimeout != 7 {
		t.Fatalf("ProxyTimeout = %d, want 7", res.ProxyTimeout)
	}
}
