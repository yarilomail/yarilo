package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/config"
)

// main refuses to start without the listener, so every row has it.
const services = "services:\n  lmtp:\n    enabled: true\n    port: 24\n"

func TestTheConfiguredProxyTimeoutReachesTheProxy(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       time.Duration
	}{
		{"set", "protocol:\n  lmtp:\n    proxy:\n      lmtp_proxy_timeout: 7\n", 7 * time.Second},
		{"the pre-beta spelling", "protocol:\n  lmtp:\n    proxy:\n      timeout: 9\n", 9 * time.Second},
		{"unset", "", 125 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "yarilo.yaml")
			if err := os.WriteFile(path, []byte(services+tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := options(cfg, "h", nil, nil).ProxyTimeout; got != tc.want {
				t.Errorf("ProxyTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}
