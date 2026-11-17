package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

// The components whose binary builds the store with keys (its imports, less
// the keyless list), whose role checks read the keys, and the containers the
// chart mounts the keys into are one set: one more, and check refuses in a
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
	// Which binaries build the store, from their imports, less the keyless ones.
	out, err := exec.CommandContext(t.Context(), "go", "list", "-f", `{{.ImportPath}} {{join .Deps " "}}`, "../...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	buildsStore := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.Contains(f[0], "/app/yarilo-") {
			continue
		}
		bin := f[0][strings.LastIndex(f[0], "/")+1:]
		buildsStore[bin] = slices.Contains(f[1:], "github.com/yarilomail/yarilo/v2/internal/storage/mailboxbuild") && keyless[bin] == ""
	}
	for bin := range keyless {
		if _, ok := buildsStore[bin]; !ok {
			t.Errorf("%s is listed keyless and is no binary", bin)
		}
	}
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
			if r, b := readsKeys(comp), buildsStore[comp]; r != mounted || b != mounted {
				t.Errorf("%s/%s (%s): builds the store with keys %v, check reads the keys %v, the chart mounts them %v",
					w.Metadata.Name, c.Name, comp, b, r, mounted)
			}
		}
	}
	for _, m := range regexp.MustCompile(`(?m)^  (yarilo-[a-z0-9-]+)\)`).FindAllStringSubmatch(readFile(t, "../../docker/entrypoint.sh"), -1) {
		if readsKeys(m[1]) && !rendered[m[1]] && !notInChart[m[1]] {
			t.Errorf("%s reads the keys and no rendered container runs it: the comparison misses it", m[1])
		}
	}
}
