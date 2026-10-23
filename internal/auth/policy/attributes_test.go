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
	Vars: map[string]string{
		"user": "alice@example.com", "protocol": "sieve", "local_ip": "198.51.100.2",
		"remote_ip": "192.0.2.1", "mechanism": "SCRAM-SHA-256", "secured": "TLS", "remote_port": "51000",
		"real_remote_ip": "10.0.0.5", "session": "s1", "client_id": `"name" "x"`,
		"domain_first": "example.com", "domain_last": "example.com", "original_user": "alice@example.com",
	},
	Passdb: map[string]string{"tenant": "acme"},
	Userdb: map[string]string{"quota_rule": "*:storage=1G"},
}

// rawBody sends one report with attrs and returns the body the server got.
func rawBody(t *testing.T, attrs map[string]string) []byte {
	t.Helper()
	srv, cap := newPolicyServer(t, 200, "")
	c, err := New(Config{URL: srv.URL, HashNonce: "n", RequestAttributes: attrs})
	if err != nil {
		t.Fatal(err)
	}
	c.ReportAfter(context.Background(), attrReq, false, false)
	return cap.body
}

func bodyFor(t *testing.T, attrs map[string]string) map[string]any {
	t.Helper()
	raw := rawBody(t, attrs)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body %q: %v", raw, err)
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

// The default body is the same bytes the reference sends: keys in order, the
// outcome and tls after them.
func TestTheDefaultBodyIsByteStable(t *testing.T) {
	c, _ := New(Config{URL: "http://x", HashNonce: "n"})
	want := `{"device_id":"\"name\" \"x\"","fail_type":"credentials","login":"alice@example.com",` +
		`"protocol":"sieve","pwhash":"` + c.hashPassword("alice@example.com", "pw") + `","remote":"192.0.2.1",` +
		`"session_id":"s1","success":false,"policy_reject":false,"tls":true}`
	if got := string(rawBody(t, nil)); got != want {
		t.Errorf("body\n %s\nwant\n %s", got, want)
	}
}

func TestAttributesAreTemplates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs map[string]string
		path  []string
		want  string
	}{
		{"a variable of the auth request", map[string]string{"m": "%{mechanism}"}, []string{"m"}, "SCRAM-SHA-256"},
		{"literals around variables", map[string]string{"w": "%{user} via %{secured}"}, []string{"w"}, "alice@example.com via TLS"},
		{"a filter", map[string]string{"d": "%{user | domain}"}, []string{"d"}, "example.com"},
		{"a chain of filters", map[string]string{"u": "%{user | username | upper}"}, []string{"u"}, "ALICE"},
		{"the real address", map[string]string{"r": "%{real_remote_ip}:%{remote_port}"}, []string{"r"}, "10.0.0.5:51000"},
		{"a passdb field", map[string]string{"t": "%{passdb:tenant}"}, []string{"t"}, "acme"},
		{"a userdb field", map[string]string{"q": "%{userdb:quota_rule}"}, []string{"q"}, "*:storage=1G"},
		{"a missing passdb field is empty", map[string]string{"t": "%{passdb:nope}"}, []string{"t"}, ""},
		{"a default for an empty value", map[string]string{"h": "%{home | default('none')}"}, []string{"h"}, "none"},
		{"%% is kept as it is", map[string]string{"p": "100%%"}, []string{"p"}, "100%%"},
		{"%%{ is a literal %{", map[string]string{"p": "%%{user}"}, []string{"p"}, "%{user}"},
		{"key/subkey nests", map[string]string{"attrs/client": "%{client_id}"}, []string{"attrs", "client"}, `"name" "x"`},
		{"deeper keys nest deeper", map[string]string{"a/b/c": "%{session}"}, []string{"a", "b", "c"}, "s1"},
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

// Values are JSON-escaped as the reference escapes them, literals included,
// and a safe value is not.
func TestValuesAreEscaped(t *testing.T) {
	for _, tc := range []struct {
		tmpl, want string
	}{
		{`%{literal('a"b\\c')}`, `"v":"a\"b\\c"`},
		{`x"y`, `"v":"x\"y"`},
		{"%{literal('\\x01\\n')}", `"v":"\u0001\n"`},
		{"%{literal('<&>')}", `"v":"<&>"`},
		{"%{literal('a\xe2\x80\xa8b')}", `"v":"a\u2028b"`},
	} {
		if got := string(rawBody(t, map[string]string{"v": tc.tmpl})); !strings.Contains(got, tc.want) {
			t.Errorf("%s: body %s, want %s in it", tc.tmpl, got, tc.want)
		}
	}
}

// Sibling objects each close before the next opens.
func TestSiblingObjectsNest(t *testing.T) {
	want := `{"a":{"x":"s1","y":"s1"},"b":{"z":"s1"},"c":"s1",`
	got := string(rawBody(t, map[string]string{"a/x": "%{session}", "a/y": "%{session}", "b/z": "%{session}", "c": "%{session}"}))
	if !strings.HasPrefix(got, want) {
		t.Errorf("body %s, want it to start %s", got, want)
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
		{"unknown filter", map[string]string{"x": "%{user | nope}"}},
		{"unterminated", map[string]string{"x": "%{user"}},
		{"empty object", map[string]string{"/x": "%{user}"}},
		{"empty subkey", map[string]string{"x/": "%{user}"}},
		{"value and object", map[string]string{"x": "%{user}", "x/y": "%{user}"}},
		{"value and deeper object", map[string]string{"x/y": "%{user}", "x/y/z": "%{user}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(Config{URL: "http://x", HashNonce: "n", RequestAttributes: tc.attrs}); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// Every variable of the table may be named.
func TestEveryVariableIsKnown(t *testing.T) {
	attrs := map[string]string{}
	for _, n := range VariableNames {
		attrs[n] = "%{" + n + "}"
	}
	if _, err := New(Config{URL: "http://x", HashNonce: "n", RequestAttributes: attrs}); err != nil {
		t.Fatal(err)
	}
}

// The config's default is the client's: an operator who sets nothing sends
// what a policy written for the reference reads.
func TestTheConfigDefaultIsTheClientDefault(t *testing.T) {
	if got := config.Defaults().Auth.Policy.RequestAttributes; !maps.Equal(got, DefaultRequestAttributes) {
		t.Errorf("config default %v, client default %v", got, DefaultRequestAttributes)
	}
}
