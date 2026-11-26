// Package master is yarilo-master: the supervisor of the FreeBSD port. It
// starts the components in order, starts again one that dies, and stops them
// in reverse (#2715). Kubernetes and the Linux packages do not run it.
package master

import (
	"fmt"
	"path/filepath"
	"strings"
)

// role is which configured user a component runs as.
type role int

const (
	roleInternal role = iota
	roleAuth
	roleLogin
	roleDelivery
	roleLog
)

// LogComponent is the log process: the supervisor's own binary in the role
// that reads every component's pipe. It is started first and stopped last.
const LogComponent = "log"

func roleOf(name string) role {
	switch {
	case name == LogComponent:
		return roleLog
	case name == "lmtp-login":
		return roleDelivery
	case strings.HasSuffix(name, "-login"):
		return roleLogin
	case name == "auth" || name == "warden":
		return roleAuth
	default:
		return roleInternal
	}
}

// telemetryOffset is each component's port above master_telemetry_port: the
// rc script's table, so the scrape targets and the port rules do not move.
var telemetryOffset = map[string]int{
	"locks": 1, "auth": 2, "warden": 3, "dict": 4, "fts": 5, "imap": 6,
	"pop3": 7, "lmtp": 8, "submission": 9, "managesieve": 10, "jmap": 11,
	"quota-status": 12, "imap-login": 13, "pop3-login": 14, "lmtp-login": 15,
	"submission-login": 16, "managesieve-login": 17, "jmap-login": 18,
	"sasl-login": 19, "backend-api": 20, LogComponent: 21,
}

// readySockets are the sockets a component serves that the ones after it
// need, relative to the run directory.
func readySockets(name string) []string {
	switch name {
	case "locks":
		return []string{"locks/locks.sock"}
	case "auth":
		return []string{"auth/auth-client.sock", "auth/auth-master.sock"}
	case "warden":
		return []string{"auth/warden.sock"}
	case "dict":
		return []string{"dict/dict.sock"}
	}
	return nil
}

// envFile is the password file a component reads, if it has one.
func envFile(name, dir string) string {
	switch name {
	case "auth", "dict":
		return filepath.Join(dir, name+".env")
	}
	return ""
}

// runDirs are the directories under the run directory and who owns them.
var runDirs = []struct {
	name string
	role role
}{
	{"auth", roleAuth},
	{"locks", roleInternal},
	{"dict", roleInternal},
}

func knownComponent(name string) error {
	if _, ok := telemetryOffset[name]; !ok || name == LogComponent {
		return fmt.Errorf("unknown component %q in master_components", name)
	}
	return nil
}
