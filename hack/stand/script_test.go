// Package stand holds the sandbox measurement scripts. The rows here are what
// CI can check about them without a cluster.
package stand

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func scripts(t *testing.T) []string {
	t.Helper()
	found, err := filepath.Glob("*.sh")
	if err != nil || len(found) == 0 {
		t.Fatalf("no scripts found: %v", err)
	}
	return found
}

// A window that dies on the tooling costs the slot, and the slot is the scarce
// thing: the scripts at least have to parse.
func TestEveryStandScriptParses(t *testing.T) {
	for _, s := range scripts(t) {
		t.Run(s, func(t *testing.T) {
			if out, err := exec.Command("bash", "-n", s).CombinedOutput(); err != nil {
				t.Errorf("bash -n: %v\n%s", err, out)
			}
		})
	}
}

// An empty array is an unbound variable under `set -u` on bash 3.2: the arm
// with no overlay died at the deploy line on a laptop that runs it.
func TestAnEmptyArrayExpansionSurvivesSetU(t *testing.T) {
	const snippet = `set -euo pipefail
a=()
printf '%s\n' cmd ${a[@]+"${a[@]}"} tail
a=(-f over.yaml)
printf '%s\n' cmd ${a[@]+"${a[@]}"} tail`
	out, err := exec.Command("bash", "-c", snippet).CombinedOutput()
	if err != nil {
		t.Fatalf("the guarded expansion did not run: %v\n%s", err, out)
	}
	got := strings.Fields(string(out))
	want := []string{"cmd", "tail", "cmd", "-f", "over.yaml", "tail"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

// The arm's deploy is the line that bit: it must not expand an array without
// the guard, or the next windowless arm dies at the same place.
func TestTheArmDeployGuardsItsOverlay(t *testing.T) {
	raw, err := os.ReadFile("ab-arm.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "[@]}\"") || strings.Contains(line, "[@]+") {
			continue
		}
		// A for-loop over a list built in the same function is fine; what is
		// not is an array that can legitimately be empty.
		if strings.Contains(line, "overlay_args") || strings.Contains(line, "curls") ||
			strings.Contains(line, "pids") || strings.Contains(line, "files") {
			t.Errorf("unguarded expansion, empty is a legitimate value here:\n  %s", strings.TrimSpace(line))
		}
	}
}
