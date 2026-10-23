package policy

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

var attrReq = Request{
	Username: "alice@example.com", Password: "pw", RemoteIP: "192.0.2.1", LocalIP: "198.51.100.2",
	Service: "managesieve", DeviceID: `"name" "x"`, SessionID: "s1", TLS: true, FailType: "credentials",
}

// bodyFor sends one report with attrs and returns the body the server got.
func bodyFor(t *testing.T, attrs map[string]string) map[string]any {
	t.Helper()
	srv, cap := newPolicyServer(t, 200, "")
	c, err := New(Config{URL: srv.URL, HashNonce: "n", RequestAttributes: attrs})
	if err != nil {
		t.Fatal(err)
	}
	c.ReportAfter(context.Background(), attrReq, false, false)
	var body map[string]any
	if err := json.Unmarshal(cap.body, &body); err != nil {
		t.Fatalf("body %q: %v", cap.body, err)
	}
	return body
}

func TestTheDefaultBodyKeepsItsKeys(t *testing.T) {
	c, _ := New(Config{URL: "http://x", HashNonce: "n"})
	body := bodyFor(t, nil)
	for _, tc := range []struct {
		key  string
		want any
	}{
		{"login", "alice@example.com"},
		{"pwhash", c.hashPassword("alice@example.com", "pw")},
		{"remote", "192.0.2.1"},
		{"device_id", `"name" "x"`},
		{"protocol", "sieve"},
		{"session_id", "s1"},
		{"fail_type", "credentials"},
		{"tls", true},
		{"success", false},
		{"policy_reject", false},
	} {
		if got := body[tc.key]; got != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.key, got, tc.want)
		}
	}
	if len(body) != 10 {
		t.Errorf("body has %d keys, want 10: %v", len(body), body)
	}
}

func TestAttributesAreTemplates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs map[string]string
		path  []string
		want  string
	}{
		{"variables and literals", map[string]string{"who": "%{username} at %{domain}"}, []string{"who"}, "alice at example.com"},
		{"local address", map[string]string{"to": "%{local_ip}"}, []string{"to"}, "198.51.100.2"},
		{"short names", map[string]string{"r": "%{rip}/%{lip}"}, []string{"r"}, "192.0.2.1/198.51.100.2"},
		{"a doubled percent", map[string]string{"p": "100%%"}, []string{"p"}, "100%"},
		{"key/subkey nests", map[string]string{"attrs/client": "%{client_id}"}, []string{"attrs", "client"}, `"name" "x"`},
		{"spaces inside braces", map[string]string{"s": "%{ session }"}, []string{"s"}, "s1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got any = bodyFor(t, tc.attrs)
			for _, k := range tc.path {
				m, ok := got.(map[string]any)
				if !ok {
					t.Fatalf("no object at %s in %v", k, got)
				}
				got = m[k]
			}
			if got != tc.want {
				t.Errorf("%s = %#v, want %q", strings.Join(tc.path, "/"), got, tc.want)
			}
		})
	}
}

// Only the configured keys are sent, with tls after them.
func TestConfiguredAttributesReplaceTheBody(t *testing.T) {
	body := bodyFor(t, map[string]string{"login": "%{user}"})
	if len(body) != 4 || body["login"] != "alice@example.com" || body["tls"] != true {
		t.Errorf("body %v, want login, tls, success and policy_reject", body)
	}
}

// A template that cannot be expanded refuses to start rather than sending
// empty values.
func TestABadAttributeIsRefusedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs map[string]string
	}{
		{"unknown variable", map[string]string{"x": "%{nope}"}},
		{"unterminated", map[string]string{"x": "%{user"}},
		{"bare percent", map[string]string{"x": "50% off"}},
		{"empty object", map[string]string{"/x": "%{user}"}},
		{"empty subkey", map[string]string{"x/": "%{user}"}},
		{"value and object", map[string]string{"x": "%{user}", "x/y": "%{user}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(Config{URL: "http://x", HashNonce: "n", RequestAttributes: tc.attrs}); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// The config's default is the client's: an operator who sets nothing sends
// what a policy written for the reference reads.
func TestTheConfigDefaultIsTheClientDefault(t *testing.T) {
	if got := config.Defaults().Auth.Policy.RequestAttributes; !maps.Equal(got, DefaultRequestAttributes) {
		t.Errorf("config default %v, client default %v", got, DefaultRequestAttributes)
	}
}
