package guard_test

import (
	"os/exec"
	"strings"
	"testing"
)

// A session binary holds no credential chain and no storage engine: both live
// behind a service, and a driver in its tree means one was built here (#1733).
//
// go-redis is not listed: pkg/locks and internal/warden speak to Redis directly
// and are not storage engines.
func TestASessionBinaryLinksNoPassdbDriver(t *testing.T) {
	forbidden := []string{
		"yarilo/internal/auth/sql",
		"yarilo/internal/auth/passwdfile",
		"yarilo/internal/auth/static",
		"yarilo/internal/auth/passdbs",
		"yarilo/pkg/dict/sql",
		"yarilo/pkg/dict/redis",
		"go-sql-driver/mysql",
		"lib/pq",
		"modernc.org/sqlite",
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
