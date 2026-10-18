package guard_test

import (
	"os/exec"
	"slices"
	"testing"
)

// The stand's values: the imapsieve scripts reach the imap container of the
// co-located backend at imapsieve_script_dir, and no other container (#2292).
func TestIMAPSieveScriptsReachTheBackendIMAP(t *testing.T) {
	out, err := exec.Command("helm", "template", "../../helm", "-f", "../../helm_values/values-sandbox.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	keys, mounts, volume := scriptsShape(t, out, "imapsieve-scripts")
	if !slices.Equal(keys, []string{"refile.sieve"}) || !volume {
		t.Errorf("ConfigMap keys %v, volume %v; want the sandbox's refile.sieve and a volume", keys, volume)
	}
	if len(mounts) != 1 || mounts["yarilo-imap"] != "/etc/yarilo/imapsieve" {
		t.Errorf("mounted in %v, want only yarilo-imap at /etc/yarilo/imapsieve", mounts)
	}
}

// No imapsieve scripts in values render nothing: no ConfigMap, volume or mount.
func TestNoIMAPSieveScriptsRenderNothing(t *testing.T) {
	out, err := renderWith(t, "")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	keys, mounts, volume := scriptsShape(t, out, "imapsieve-scripts")
	if keys != nil || len(mounts) != 0 || volume {
		t.Errorf("with no scripts: ConfigMap keys %v, mounts %v, volume %v", keys, mounts, volume)
	}
}
