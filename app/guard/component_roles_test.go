package guard_test

import (
	"regexp"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// yarctl config check without -r takes the role from YARILO_COMPONENT: every
// component the entrypoint starts must name its role checks, or check in that
// container refuses a valid file.
func TestEveryComponentHasItsRoleChecks(t *testing.T) {
	re := regexp.MustCompile(`(?m)^  (yarilo-[a-z0-9-]+)\)`)
	found := re.FindAllStringSubmatch(readFile(t, "../../docker/entrypoint.sh"), -1)
	if len(found) < 5 {
		t.Fatalf("found %d components in the entrypoint, which cannot be right", len(found))
	}
	for _, m := range found {
		roles, err := config.ComponentRoles(m[1])
		if err != nil {
			t.Errorf("%s: %v", m[1], err)
			continue
		}
		for _, r := range roles {
			if err := config.Defaults().CheckRole(r); err != nil && regexp.MustCompile(`no role`).MatchString(err.Error()) {
				t.Errorf("%s maps to %s, which is no role", m[1], r)
			}
		}
	}
}
