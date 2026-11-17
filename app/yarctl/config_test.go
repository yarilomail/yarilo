package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

const testConfig = `storage:
  mail_driver: sdbox
director_service:
  ring_secret: s3cret-ring
backend_api:
  token: api-t0ken
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runConfig(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := dispatchConfig(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestConfigCalls(t *testing.T) {
	path := writeConfig(t, testConfig)
	cases := []struct {
		name    string
		args    []string
		has     []string
		hasNot  []string
		wantErr bool
	}{
		{"differences only", []string{"-c", path}, []string{"mail_driver: sdbox", "ring_secret: " + config.Hidden}, []string{"ssl_min_protocol", "s3cret-ring"}, false},
		{"every key", []string{"-c", path, "-a"}, []string{"mail_driver: sdbox", "ssl_min_protocol: TLS1.2"}, []string{"s3cret-ring", "api-t0ken"}, false},
		{"secrets shown", []string{"-c", path, "-a", "-P"}, []string{"s3cret-ring", "api-t0ken"}, []string{config.Hidden}, false},
		{"defaults", []string{"-c", filepath.Join(t.TempDir(), "absent.yaml"), "-d"}, []string{"ssl_min_protocol: TLS1.2", "# yarilo "}, []string{"sdbox"}, false},
		{"a key at its default", []string{"-c", path, "general.ssl.ssl_min_protocol"}, []string{"ssl_min_protocol: TLS1.2"}, []string{"mail_driver"}, false},
		{"a section", []string{"-c", path, "general.ssl"}, []string{"ssl_min_protocol", "ssl_server_cert_file"}, []string{"mail_driver"}, false},
		{"values only", []string{"-c", path, "-h", "storage.mail_driver", "director_service.ring_secret"}, []string{"sdbox\n" + config.Hidden + "\n"}, []string{"mail_driver:", "# yarilo"}, false},
		{"values only, secret shown", []string{"-c", path, "-h", "-P", "director_service.ring_secret"}, []string{"s3cret-ring\n"}, nil, false},
		{"flags after the key", []string{"-c", path, "storage.mail_driver", "-h"}, []string{"sdbox\n"}, []string{"mail_driver:", "# yarilo"}, false},
		{"check passes", []string{"-c", path, "-r", "yarilo-fts", "check"}, nil, []string{"\n"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errOut, err := runConfig(t, c.args...)
			if (err != nil) != c.wantErr {
				t.Fatalf("err %v, stderr %q", err, errOut)
			}
			body := out
			if slices.Contains(c.args, "-h") {
				if !strings.HasPrefix(errOut, "# yarilo ") {
					t.Fatalf("with -h the version line is not on stderr: %q", errOut)
				}
			} else {
				if !strings.HasPrefix(out, "# yarilo ") {
					t.Fatalf("first line %q", strings.SplitN(out, "\n", 2)[0])
				}
				body = strings.SplitN(out, "\n", 2)[1]
			}
			for _, h := range c.has {
				if !strings.Contains(out, h) {
					t.Errorf("no %q in:\n%s", h, out)
				}
			}
			for _, h := range c.hasNot {
				if strings.Contains(body, h) {
					t.Errorf("%q in:\n%s", h, out)
				}
			}
		})
	}
}

// check refuses a bad file with the error a server start gives, after the
// version line, with a non-zero exit.
func TestConfigCheckRefusesAsAServer(t *testing.T) {
	bad := writeConfig(t, "storage:\n  mail_fsync: bogus\n")
	_, serverErr := config.Load(bad)
	if serverErr == nil {
		t.Fatal("the bad file loads")
	}
	out, errOut, err := runConfig(t, "-c", bad, "check")
	if !errors.Is(err, errConfigRefused) {
		t.Fatalf("err %v", err)
	}
	if !strings.HasPrefix(out, "# yarilo ") || !strings.Contains(out, bad) {
		t.Fatalf("no version line before the error: %q", out)
	}
	if strings.TrimSpace(errOut) != serverErr.Error() {
		t.Fatalf("check said %q, a server says %q", strings.TrimSpace(errOut), serverErr)
	}

	// A role check, not only the loader's.
	role := writeConfig(t, "fts:\n  language_tokenizer_generic_algorithm: nonsense\n")
	if _, errOut, err := runConfig(t, "-c", role, "check"); err == nil || !strings.Contains(errOut, "yarilo-fts") {
		t.Fatalf("a role refusal passed: %v %q", err, errOut)
	}
}

// The output of -a -P loads back into the same configuration.
func TestConfigOutputLoadsBack(t *testing.T) {
	path := writeConfig(t, testConfig)
	out, _, err := runConfig(t, "-c", path, "-a", "-P")
	if err != nil {
		t.Fatal(err)
	}
	again := writeConfig(t, out)
	want, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(again)
	if err != nil {
		t.Fatalf("the output does not load: %v", err)
	}
	a, b := values(want), values(got)
	for k, v := range a {
		if !sameValue(v, b[k]) {
			t.Errorf("%s: %v, after the round trip %v", k, v, b[k])
		}
	}
}

func values(c *config.Config) map[string]any {
	m := map[string]any{}
	for _, s := range config.Walk(c, false) {
		if !s.Alias {
			m[s.Key] = s.Value
		}
	}
	return m
}

// Every key the settings export lists is in -a: one walk serves both.
func TestConfigIsComplete(t *testing.T) {
	out, _, err := runConfig(t, "-d", "-a", "-h")
	if err != nil {
		t.Fatal(err)
	}
	shown := map[string]bool{}
	var unset []string // sections shown as null: every key under them is accounted for
	for _, s := range visible(config.Walk(config.Defaults(), false)) {
		shown[s.Key] = true
		if s.Kind == "section" {
			unset = append(unset, s.Key+".")
		}
	}
	for _, s := range config.Walk(config.Defaults(), true) {
		if s.Alias || strings.ContainsAny(s.Key, "*[") || shown[s.Key] {
			continue
		}
		under := false
		for _, u := range unset {
			under = under || strings.HasPrefix(s.Key, u)
		}
		if !under {
			t.Errorf("%s is exported and not shown", s.Key)
		}
	}
	if len(strings.Split(strings.TrimSpace(out), "\n")) < len(shown) {
		t.Fatalf("-a -h printed fewer lines than keys")
	}
}

// sameValue is DeepEqual with an empty list or map equal to a nil one: YAML
// writes both as [] or {}, and the defaults are filled before the file.
func sameValue(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.IsValid() && vb.IsValid() && va.Type() == vb.Type() &&
		(va.Kind() == reflect.Slice || va.Kind() == reflect.Map) {
		return va.Len() == 0 && vb.Len() == 0
	}
	return false
}

// A key that does not exist is an error, not an empty answer a script reads.
func TestConfigUnknownKeyIsAnError(t *testing.T) {
	path := writeConfig(t, testConfig)
	for _, args := range [][]string{
		{"-c", path, "storage.mail_drivr"},
		{"-c", path, "-h", "storage.mail_drivr"},
		{"-c", path, "storage.mail_driver", "no_such_section"},
	} {
		out, errOut, err := runConfig(t, args...)
		if !errors.Is(err, errConfigRefused) || !strings.Contains(errOut, "no such key") {
			t.Fatalf("%v: err %v, stderr %q, out %q", args, err, errOut, out)
		}
	}
}

// check refuses key files a store-building start would refuse, with the
// error that start gives.
func TestConfigCheckReadsTheCryptKeys(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "public")
	path := writeConfig(t, "storage:\n  mail_crypt:\n    write: age\n    public_key: "+missing+"\nlocks_client:\n  mode: remote\n  endpoints: [\"l:9104\"]\nbackend_api:\n  token: t\n")
	if _, err := config.Load(path); err != nil {
		t.Fatalf("the file itself is valid: %v", err)
	}
	for _, role := range []string{"yarilo-backend", "yarilo-backend-api", "yarilo-jmap", "yarilo-fts"} {
		_, errOut, err := runConfig(t, "-c", path, "-r", role, "check")
		if err == nil || !strings.Contains(errOut, "storage.mail_crypt.public_key") {
			t.Errorf("%s: err %v, stderr %q", role, err, errOut)
		}
	}
	// Without -r: the locks_client section says the backend role is here.
	if _, errOut, err := runConfig(t, "-c", path, "check"); err == nil || !strings.Contains(errOut, "storage.mail_crypt.public_key") {
		t.Errorf("default roles: err %v, stderr %q", err, errOut)
	}
	// A role that builds no store does not hold the keys.
	if _, errOut, err := runConfig(t, "-c", path, "-r", "yarilo-director", "check"); err != nil && strings.Contains(errOut, "mail_crypt") {
		t.Errorf("director refused for the keys: %q", errOut)
	}
}

// x=$(yarctl config -h KEY) is the value and nothing else.
func TestConfigValuesOnlyIsTheValueAlone(t *testing.T) {
	path := writeConfig(t, testConfig)
	out, errOut, err := runConfig(t, "-c", path, "-h", "storage.mail_driver")
	if err != nil {
		t.Fatal(err)
	}
	if out != "sdbox\n" {
		t.Fatalf("stdout %q, want the value alone", out)
	}
	if !strings.Contains(errOut, path) {
		t.Fatalf("the version line is gone: stderr %q", errOut)
	}
}
