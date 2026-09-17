package guard_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The session binaries authenticate through the auth service, so a passdb in
// their dependency tree is a chain built where none should be (#1733).
//
// The SQL client drivers stay for now: pkg/dict/sql still links one, which the
// dict proxy removes (#1733 PR 3).
func TestASessionBinaryLinksNoPassdbDriver(t *testing.T) {
	forbidden := []string{
		"yarilo/internal/auth/sql",
		"yarilo/internal/auth/passwdfile",
		"yarilo/internal/auth/static",
		"yarilo/internal/auth/passdbs",
	}
	for _, bin := range []string{"yarilo-imap", "yarilo-pop3", "yarilo-submission", "yarilo-lmtp", "yarilo-managesieve", "yarilo-jmap"} {
		t.Run(bin, func(t *testing.T) {
			out, err := exec.Command("go", "list", "-deps", "../../app/"+bin).Output()
			if err != nil {
				t.Fatalf("go list -deps %s: %v", bin, err)
			}
			var found []string
			for _, dep := range strings.Split(string(out), "\n") {
				for _, bad := range forbidden {
					if strings.Contains(dep, bad) {
						found = append(found, dep)
					}
				}
			}
			if len(found) > 0 {
				t.Errorf("%s links %d passdb packages: %s", bin, len(found), strings.Join(found, " "))
			}
		})
	}
}
