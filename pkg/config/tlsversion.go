package config

import (
	"crypto/tls"
	"fmt"
	"reflect"
	"strings"
)

// ParseMinTLS reads ssl_min_protocol: TLS1.2 or TLS1.3, also spelt TLSv1.2 and
// TLSv1.3 as the reference writes them; empty is TLS1.2, the default. Anything
// else is refused: read as TLS1.2, a typo of TLS1.3 lowered the floor (#2568).
func ParseMinTLS(s string) (uint16, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "", "TLS1.2", "TLSV1.2":
		return tls.VersionTLS12, nil
	case "TLS1.3", "TLSV1.3":
		return tls.VersionTLS13, nil
	}
	return 0, fmt.Errorf("ssl_min_protocol %q: want TLS1.2 or TLS1.3", s)
}

// checkMinTLS refuses at load every ssl_min_protocol that does not parse:
// general.ssl and the ssl block of every service, each named by its key.
func checkMinTLS(c *Config) error {
	if _, err := ParseMinTLS(c.General.SSL.SSLMinProtocol); err != nil {
		return fmt.Errorf("config: general.ssl.%w", err)
	}
	sv := reflect.ValueOf(c.Services)
	st := sv.Type()
	for i := range st.NumField() {
		svc, ok := sv.Field(i).Interface().(*ServiceConfig)
		if !ok || svc == nil || svc.SSL == nil {
			continue
		}
		name, _, _ := strings.Cut(st.Field(i).Tag.Get("koanf"), ",")
		if _, err := ParseMinTLS(svc.SSL.SSLMinProtocol); err != nil {
			return fmt.Errorf("config: services.%s.ssl.%w", name, err)
		}
	}
	return nil
}
