package master

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// HostPreStart refuses a login proxy the host has not allowed its low port:
// without the mac_portacl rule its bind fails with EPERM, and saying why
// before it starts beats a crash loop. The uid is the configured user's, so
// the check and the configuration cannot drift apart.
func HostPreStart(c Component) error {
	if !strings.HasSuffix(c.Name, "-login") {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "kldstat", "-q", "-m", "mac_portacl").Run() != nil {
		return fmt.Errorf("mac_portacl is not loaded; the login proxies cannot bind their ports (see pkg-message)")
	}
	high, err := exec.CommandContext(ctx, "sysctl", "-n", "net.inet.ip.portrange.reservedhigh").Output()
	if err != nil || strings.TrimSpace(string(high)) != "0" {
		return fmt.Errorf("net.inet.ip.portrange.reservedhigh is not 0; mac_portacl does not decide the low ports (see pkg-message)")
	}
	rules, err := exec.CommandContext(ctx, "sysctl", "-n", "security.mac.portacl.rules").Output()
	if err != nil {
		return fmt.Errorf("security.mac.portacl.rules: %w", err)
	}
	if !strings.Contains(","+strings.TrimSpace(string(rules))+",", fmt.Sprintf(",uid:%d:tcp:", c.Account.UID)) {
		return fmt.Errorf("security.mac.portacl.rules names no port for %s (uid %d) (see pkg-message)", c.Account.Name, c.Account.UID)
	}
	return nil
}
