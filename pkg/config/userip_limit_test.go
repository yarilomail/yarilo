package config

import (
	"strings"
	"testing"
)

// A protocol's own limit wins over general.limits; left out it takes
// general's, and 0 lifts it for that protocol alone (#2457).
func TestTheUserIPLimitResolvesPerProtocol(t *testing.T) {
	cfg, err := loadYAML(t, `
general:
  limits:
    mail_max_userip_connections: 10
protocol:
  imap:
    mail_max_userip_connections: 0
  jmap:
    mail_max_userip_connections: 4
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		protocol string
		want     int
	}{
		{"imap", 0}, {"jmap", 4}, {"pop3", 10}, {"submission", 10}, {"lmtp", 10}, {"managesieve", 10},
		{"JMAP", 4}, {"", 10},
	} {
		if got := cfg.MaxUserIPConnections(tc.protocol); got != tc.want {
			t.Errorf("MaxUserIPConnections(%q) = %d, want %d", tc.protocol, got, tc.want)
		}
	}
}

func TestANegativeProtocolUserIPLimitIsRefused(t *testing.T) {
	for _, p := range UserIPProtocols {
		_, err := loadYAML(t, "protocol:\n  "+p+":\n    mail_max_userip_connections: -1\n")
		if err == nil || !strings.Contains(err.Error(), "protocol."+p+".mail_max_userip_connections") {
			t.Errorf("%s: err = %v, want a refusal naming the key", p, err)
		}
	}
}
