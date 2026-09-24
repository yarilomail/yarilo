// Package stand holds the sandbox measurement scripts. The rows here are what
// CI can check about them without a cluster.
package stand

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		"maildir_uidlist_read_total",
		"maildir_dir_read_total",
		"maildir_listing_miss_total",
		"maildir_cache_stat_total",
		"maildir_window_closed_total",
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

// An overlay may carry only the keys its own question needs; the guard reads
// two levels, because a sibling one level down is what quietly moves in.
func TestEveryOverlayCarriesOnlyItsOwnKeys(t *testing.T) {
	cases := []struct {
		name  string
		allow string
	}{
		{name: "blockprofile", allow: `^telemetry(\.pprof)?$`},
		{name: "fsync-never", allow: `^storage(\.mail_fsync)?$`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join("..", "..", "helm_values", "values-sandbox-"+tc.name+".yaml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("the %s overlay is missing: %v", tc.name, err)
			}
			allow := regexp.MustCompile(tc.allow)
			var top string
			for _, line := range strings.Split(string(raw), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				indent := len(line) - len(strings.TrimLeft(line, " "))
				if indent > 2 {
					continue // the guard reads two levels, as the overlays are two deep
				}
				key := strings.TrimSuffix(strings.Fields(trimmed)[0], ":")
				path := key
				if indent == 2 {
					path = top + "." + key
				} else {
					top = key
				}
				if !allow.MatchString(path) {
					t.Errorf("%s carries %q, which its question does not need", tc.name, path)
				}
			}
			// And the arm must know the name, or the overlay is unreachable.
			if !strings.Contains(armSource(t), tc.name+")") {
				t.Errorf("ab-arm.sh does not name the %s overlay", tc.name)
			}
		})
	}
}

// An overlay nobody named is refused rather than deployed silently.
func TestTheArmRefusesAnUnknownOverlay(t *testing.T) {
	if !strings.Contains(armSource(t), "no overlay is named") {
		t.Error("an unknown overlay name does not stop the arm")
	}
}

// bash resolves a function when the line runs: a helper defined below its
// first top-level caller is "command not found", and cost win406 an arm.
func TestEveryHelperIsDefinedBeforeItIsCalled(t *testing.T) {
	src := armSource(t)
	lines := strings.Split(src, "\n")
	def := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\)\s*\{`)

	defined := map[string]int{}
	for i, line := range lines {
		if m := def.FindStringSubmatch(line); m != nil {
			defined[m[1]] = i
		}
	}
	if len(defined) == 0 {
		t.Fatal("no helpers found, so this asserts nothing")
	}

	inFunc := false
	for i, line := range lines {
		if def.MatchString(line) {
			inFunc = true
			continue
		}
		if inFunc {
			if line == "}" {
				inFunc = false
			}
			continue
		}
		for name, at := range defined {
			if at <= i {
				continue
			}
			call := regexp.MustCompile(`(^|[^\w.-])` + regexp.QuoteMeta(name) + `($|[^\w.-])`)
			if call.MatchString(line) && !strings.Contains(line, "#") {
				t.Errorf("line %d calls %s, which is defined at line %d:\n  %s",
					i+1, name, at+1, strings.TrimSpace(line))
			}
		}
	}
}

// A definition inside a branch exists only when that branch runs: the carried
// arm skipped the wipe and lost two helpers defined between its steps.
func TestNoHelperIsDefinedInsideABranch(t *testing.T) {
	def := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\)\s*\{`)
	depth, inFunc := 0, false
	for i, line := range strings.Split(armSource(t), "\n") {
		trimmed := strings.TrimSpace(line)
		if def.MatchString(line) {
			if depth > 0 {
				t.Errorf("line %d defines %s inside a branch, so an arm that takes the other one has no such helper",
					i+1, strings.TrimSuffix(trimmed, "() {"))
			}
			inFunc = true
			continue
		}
		if inFunc {
			if line == "}" {
				inFunc = false
			}
			continue
		}
		// Only top-level control counts: what is inside a function travels
		// with it.
		switch {
		case strings.HasPrefix(trimmed, "if ") || trimmed == "if", strings.HasPrefix(trimmed, "for "), strings.HasPrefix(trimmed, "while "), strings.HasPrefix(trimmed, "case "):
			depth++
		case trimmed == "fi" || trimmed == "done" || trimmed == "esac":
			if depth > 0 {
				depth--
			}
		}
	}
}

// A forward that never came up is a flake and is retried once; a pod that
// cannot be profiled still stops the arm.
func TestAFlakyForwardIsRetriedOnce(t *testing.T) {
	src := armSource(t)
	if !strings.Contains(src, "retry_capture") {
		t.Fatal("no retry around the capture; one flaky forward ends an arm")
	}
	for _, kind := range []string{"block_profile", "cpu_profile"} {
		if !regexp.MustCompile(kind + `\(\) \{ retry_capture`).MatchString(src) {
			t.Errorf("%s does not go through the retry", kind)
		}
	}
	// The retry must tell the two causes apart, or a pod with no pprof at all
	// is tried twice and still fails the arm twice as slowly.
	if !strings.Contains(src, `[ "$rc" = 2 ] || return "$rc"`) {
		t.Error("the retry does not distinguish a forward that never came up from a capture that failed")
	}
}

// The method is read in the README, so that is where the carried mode's
// limits have to be: a window is set up from it, not from the shell (#1714).
func TestTheReadmeSaysWhatTheCarriedModeIsFor(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("the stand has no README: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"YARILO_ARM_KEEP_STORE",
		"first listing",
		"not** a throughput comparison",
		"bigger store",
		"two arms that both wipe and fill",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the README does not say %q, so the next window can read the mode as a general A/B", want)
		}
	}
	// And the script points at it rather than repeating it.
	if !strings.Contains(armSource(t), "README.md") {
		t.Error("the arm does not point at the README")
	}
}

// One run per type cannot show a cold listing: there is nothing to compare
// the first run with.
func TestTheArmCanRepeatARunPerType(t *testing.T) {
	src := armSource(t)
	if !strings.Contains(src, `REPEATS="${YARILO_ARM_REPEATS:-1}"`) {
		t.Fatal("an arm cannot repeat a run, so cold and warm cannot be told apart")
	}
	if !strings.Contains(src, `name="$type-run$run"`) {
		t.Error("the runs of one type share a name, so their counters are one number")
	}
	// Default stays one run, or every window pays three times over.
	if !strings.Contains(src, "YARILO_ARM_REPEATS:-1") {
		t.Error("the default is not one run")
	}
}
