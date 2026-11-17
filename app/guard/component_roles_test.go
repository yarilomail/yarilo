package guard_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// The components whose role checks read the encryption keys are exactly the
// containers the chart mounts the keys into: one more, and check refuses in a
// container the process starts in; one fewer, and check passes where the start
// exits. Which roles read the keys is asked of the checks themselves.
func TestComponentKeyChecksMatchTheKeyMounts(t *testing.T) {
	f := filepath.Join(t.TempDir(), "yarilo.yaml")
	body := "storage:\n  mail_crypt:\n    write: age\n    public_key: " + filepath.Join(t.TempDir(), "absent") + "\nlocks_client:\n  mode: remote\n  endpoints: [\"l:9104\"]\nbackend_api:\n  token: t\n"
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	readsKeys := func(component string) bool {
		roles, err := config.ComponentRoles(component)
		if err != nil {
			t.Fatalf("%s: %v", component, err)
		}
		for _, r := range roles {
			if err := cfg.CheckRole(r); err != nil && strings.Contains(err.Error(), "storage.mail_crypt") {
				return true
			}
		}
		return false
	}
	// Run by hand against a store, not by the chart.
	notInChart := map[string]bool{"yarilo-migrate": true}

	ws := renderWorkloads(t, "-f", "../../helm_values/values-sandbox.yaml", "--set", "mailCrypt.secretName=keys")
	ws = append(ws, renderWorkloads(t, "--set", "mailCrypt.secretName=keys",
		"--set", "components.backend.coLocated=false", "--set", "components.fts.enabled=true",
		"--set", "components.submission.enabled=true", "--set", "components.jmap.enabled=true")...)
	rendered := map[string]bool{}
	for _, w := range ws {
		for _, c := range w.Spec.Template.Spec.Containers {
			comp := ""
			for _, e := range c.Env {
				if e.Name == "YARILO_COMPONENT" {
					comp = e.Value
				}
			}
			if comp == "" {
				continue
			}
			rendered[comp] = true
			mounted := false
			for _, m := range c.VolumeMounts {
				mounted = mounted || m.Name == "mail-crypt"
			}
			if r := readsKeys(comp); r != mounted {
				t.Errorf("%s/%s (%s): check reads the keys %v, the chart mounts them %v", w.Metadata.Name, c.Name, comp, r, mounted)
			}
		}
	}
	for _, m := range regexp.MustCompile(`(?m)^  (yarilo-[a-z0-9-]+)\)`).FindAllStringSubmatch(readFile(t, "../../docker/entrypoint.sh"), -1) {
		if readsKeys(m[1]) && !rendered[m[1]] && !notInChart[m[1]] {
			t.Errorf("%s reads the keys and no rendered container runs it: the comparison misses it", m[1])
		}
	}
}
