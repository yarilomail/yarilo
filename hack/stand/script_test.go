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

// armSource is the arm as CI can read it: what it greps for, what it prints,
// and which steps a carried arm skips.
func armSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("ab-arm.sh")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A counter the arm collects and never prints is a number nobody reads, which
// is how the CRC price went missing from a whole window (#1964).
func TestEveryCollectedCounterHasAReader(t *testing.T) {
	src := armSource(t)
	for _, metric := range []string{
		"index_cache_record_crc_mismatch_total",
		"mailbox_message_opened_total",
		"fileindex_journal_write_failed_total",
		"mailbox_write_failed_total",
		"maildir_partial_pass_empty_total",
		"quota_folders_opened_total",
	} {
		if strings.Count(src, metric) < 2 {
			t.Errorf("%s is collected but never summed into a line an operator reads", metric)
		}
	}
}

// A carried arm must not wipe, seed or fill: those are what a window about
// state written by the version before cannot survive (#1714).
func TestACarriedArmSkipsTheWipe(t *testing.T) {
	src := armSource(t)
	if !strings.Contains(src, `KEEP_STORE="${YARILO_ARM_KEEP_STORE:-0}"`) {
		t.Fatal("the carried mode is gone; a window over old state cannot be run")
	}
	carry := strings.Index(src, `if [ "$KEEP_STORE" = "1" ]; then`)
	wipe := strings.Index(src, `step "wipe"`)
	if carry < 0 || wipe < 0 || carry > wipe {
		t.Fatal("the wipe is not inside the branch the carried mode skips")
	}
	if !strings.Contains(src, "messages-last-arm.txt") {
		t.Error("nothing records what an arm ended on, so a carried start cannot be proven")
	}
}

// The CPU profile is what prices a read, so it is taken on every arm and its
// absence fails the arm rather than passing quietly.
func TestTheCPUProfileIsTakenOnEveryArm(t *testing.T) {
	src := armSource(t)
	if !strings.Contains(src, "cpu_profile \"$ARM-$name\"") {
		t.Fatal("no arm takes a CPU profile")
	}
	idx := strings.Index(src, "cpu_profile \"$ARM-$name\"")
	before := src[:idx]
	if strings.LastIndex(before, `if [ "$BLOCKPROFILE" = "1" ]; then`) > strings.LastIndex(before, "\n  backend_counters") {
		t.Error("the CPU profile is inside the overlay branch, so an ordinary arm takes none")
	}
	if !strings.Contains(src, "this arm cannot price a read") {
		t.Error("a missing CPU profile does not fail the arm")
	}
}
