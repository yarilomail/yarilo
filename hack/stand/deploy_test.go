package stand

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeTools puts a helm and a kubectl on PATH that record their arguments in
// calls and answer what stand_deploy asks of a settled one-node stand.
func fakeTools(t *testing.T, releaseStatus string) (bin, calls string) {
	t.Helper()
	bin = t.TempDir()
	calls = filepath.Join(bin, "calls")
	helm := `#!/usr/bin/env bash
echo "helm $*" >> "` + calls + `"
case "$*" in
  *" history "*) echo '[{"revision":7,"status":"` + releaseStatus + `"}]' ;;
  *" template "*) echo '        image: "ghcr.io/x/yarilo:t1"' ;;
esac
`
	kubectl := `#!/usr/bin/env bash
echo "kubectl $*" >> "` + calls + `"
case "$*" in
  *"apply -f -"*) cat >> "` + calls + `" ;;
  *"spec.nodeName"*) echo node-1 ;;
  *"component=backend -o json") echo '{"items":[{"metadata":{"name":"yarilo-backend-0","creationTimestamp":"2026-11-19T19:00:00Z"},"status":{"conditions":[{"type":"PodScheduled","lastTransitionTime":"2026-11-19T19:00:14Z"},{"type":"Ready","lastTransitionTime":"2026-11-19T19:00:25Z"}]}}]}' ;;
  *"status.phase"*) echo Succeeded ;;
  *"spec.replicas"*) echo 1 ;;
  *'type=="Ready"'*) echo True ;;
esac
`
	for name, body := range map[string]string{"helm": helm, "kubectl": kubectl} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin, calls
}

func runDeploy(t *testing.T, bin, chart string) error {
	t.Helper()
	script := `set -u; NS=sb; KCFG=k; kube() { kubectl -n "$NS" "$@"; }; . ./deploy.sh; stand_deploy "$1" t1 "$2" -f v.yaml`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "deploy", t.TempDir(), chart)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	return cmd.Run()
}

// The rollout step upgrades the chart it is handed and no other: the arm, the
// acceptance run and the window's restore each name theirs (#2606 review).
func TestTheRolloutStepUpgradesTheChartItIsHanded(t *testing.T) {
	bin, calls := fakeTools(t, "deployed")
	if err := runDeploy(t, bin, "/charts/release-x/helm"); err != nil {
		t.Fatalf("stand_deploy: %v", err)
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var upgrades []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, " upgrade ") {
			upgrades = append(upgrades, line)
		}
	}
	if len(upgrades) != 1 || !strings.Contains(upgrades[0], " upgrade yarilo /charts/release-x/helm ") ||
		!strings.Contains(upgrades[0], "--set image.tag=t1") {
		t.Errorf("upgrades %q, want one of /charts/release-x/helm at t1", upgrades)
	}
	if !strings.Contains(string(raw), "image: ghcr.io/x/yarilo:t1") || !strings.Contains(string(raw), "delete pod yarilo-prepull-1") {
		t.Errorf("the image was not pulled ahead by a pod that was then removed:\n%s", raw)
	}
	if strings.Index(string(raw), "apply -f -") > strings.Index(string(raw), " upgrade ") {
		t.Error("the upgrade ran before the image was pulled")
	}
	if !strings.Contains(string(raw), "-l app.kubernetes.io/component=backend -o json") {
		t.Error("the backend pods' pending and ready times were not read after the rollout")
	}

}

// A release a cut-off helm left pending stops the step before anything runs.
func TestTheRolloutStepStopsOnAPendingRelease(t *testing.T) {
	bin, calls := fakeTools(t, "pending-rollback")
	if err := runDeploy(t, bin, "/charts/x/helm"); err == nil {
		t.Fatal("stand_deploy went on over a pending release")
	}
	raw, _ := os.ReadFile(calls)
	if strings.Contains(string(raw), " upgrade ") || strings.Contains(string(raw), "apply") {
		t.Errorf("something ran over a pending release:\n%s", raw)
	}
}

// Each caller hands the step the chart it deployed before: the arm its
// $CHART, the acceptance run and the window's restore this checkout's.
func TestEveryRolloutHandsTheStepItsChart(t *testing.T) {
	for file, want := range map[string]string{
		"ab-arm.sh":           `stand_deploy "$OUT/deploy-$ARM" "$TAG" "$CHART/helm"`,
		"accept.sh":           `stand_deploy "$OUT" "$TAG" "$REPO/helm"`,
		"mailcrypt-window.sh": `stand_deploy "$OUT/deploy-restore" "$TAG" "$REPO/helm"`,
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s does not hand the step its chart: want %s", file, want)
		}
		if strings.Count(string(raw), "helm upgrade")+strings.Count(string(raw), "upgrade yarilo") != 0 {
			t.Errorf("%s runs helm upgrade itself; it goes through stand_deploy", file)
		}
	}
	raw, _ := os.ReadFile("deploy.sh")
	if strings.Contains(string(raw), `"$REPO/helm"`) || strings.Contains(string(raw), `"$CHART/helm"`) {
		t.Error("deploy.sh names a chart of its own; it upgrades the one it is handed")
	}
}

// The watcher counts the seconds a backend pod is not Ready, elapsed time and
// not looks, and the Ready one stays at zero.
func TestTheWatcherCountsTheSecondsABackendIsDown(t *testing.T) {
	file := filepath.Join(t.TempDir(), "down")
	script := `kube() { case "$*" in *replicas*) echo 2 ;; *yarilo-backend-1*) echo False ;; *) echo True ;; esac; }
. ./deploy.sh; stand_watch_backend "$1" & w=$!; sleep 3; kill $w; wait $w`
	if err := exec.CommandContext(t.Context(), "bash", "-c", script, "watch", file).Run(); err != nil {
		t.Fatalf("watcher: %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(raw))
	if len(got) != 4 || got[0] != "yarilo-backend-0" || got[1] != "0" || got[2] != "yarilo-backend-1" || got[3] < "2" {
		t.Errorf("watcher wrote %q, want backend-0 at 0 and backend-1 at 2 s or more", raw)
	}
}

// A stand script runs from a laptop too, whose bash is 3.2: none needs bash 4.
func TestNoStandScriptNeedsBash4(t *testing.T) {
	bash4 := regexp.MustCompile(`declare -A|local -A|\bmapfile\b|\breadarray\b|\$\{[a-zA-Z_]+(,,|\^\^)\}`)
	for _, f := range scripts(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if bash4.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "#") {
				t.Errorf("%s:%d needs bash 4: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
