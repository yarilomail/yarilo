package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A key nothing reads stops the load, at any depth; the operator's free-form
// maps and a retired key do not.
func TestUnknownKeysRefused(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"top level", "not_a_section:\n  x: 1\n", "not_a_section"},
		{"in a section", "storage:\n  mail_drivr: sdbox\n", "storage.mail_drivr"},
		{"in a service block", "services:\n  imap:\n    ssl:\n      ssl_min_proto: TLS1.3\n", "services.imap.ssl.ssl_min_proto"},
		{"in a map entry", "dicts:\n  metadata:\n    driver: file\n    drivr: x\n", "dicts.metadata.drivr"},
		{"in a list entry", "auth:\n  passdb:\n    - driver: static\n      drver: y\n", "auth.passdb[0].drver"},
		{"free-form maps", "dicts:\n  metadata:\n    driver: file\n    settings:\n      anything: goes\nauth:\n  passdb:\n    - driver: static\n      fields:\n        free: ok\n", ""},
		{"a retired key", "telemetry:\n  telemetry_pprof_heap_enabled: true\n", ""},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "yarilo.yaml")
		if err := os.WriteFile(p, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err %v, want one naming %s", c.name, err, c.want)
		}
	}
}
