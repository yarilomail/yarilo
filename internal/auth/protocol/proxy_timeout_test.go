package protocol

import "testing"

// The auth service passes the value on as it came, readable or not.
func TestExtractProxyTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"absent", nil, ""},
		{"passdb column", map[string]string{"proxy_timeout": "7"}, "7"},
		{"userdb-scoped wins", map[string]string{"proxy_timeout": "7", "userdb_proxy_timeout": "9"}, "9"},
		{"interval", map[string]string{"proxy_timeout": "30s"}, "30s"},
		{"unreadable", map[string]string{"proxy_timeout": "abc"}, "abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFields()
			for k, v := range tc.fields {
				f.Set(k, v)
			}
			if got := extractProxyTimeout(f); got != tc.want {
				t.Fatalf("extractProxyTimeout = %q, want %q", got, tc.want)
			}
		})
	}
}

// The USER answer keeps every field when proxy_timeout is unreadable.
func TestAnUnreadableProxyTimeoutKeepsTheUserdbAnswer(t *testing.T) {
	info, err := ParseUserInfo([]string{"proxy_timeout=abc", "director_tag=t1"})
	if err != nil {
		t.Fatalf("ParseUserInfo: %v", err)
	}
	if info.ProxyTimeout != "abc" || info.DirectorTag != "t1" {
		t.Fatalf("proxy_timeout = %q, director_tag = %q; want abc, t1", info.ProxyTimeout, info.DirectorTag)
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
	if !ApplyAuthOKToken(res, "proxy_timeout=7") || res.ProxyTimeout != "7" {
		t.Fatalf("ProxyTimeout = %q, want 7", res.ProxyTimeout)
	}
}
