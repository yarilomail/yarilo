package stand

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func scenarioSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("crypt-scenarios.sh")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The counts come from the table's own total, so a folder whose name holds a
// space does not shift a column; --all reads its ALL USERS line.
func TestTheScenarioCountsReadTheTotals(t *testing.T) {
	fn := regexp.MustCompile(`(?s)\ncounts\(\) \{.*?\n\}\n`).FindString(scenarioSource(t))
	if fn == "" {
		t.Fatal("no counts() in crypt-scenarios.sh")
	}
	for _, tc := range []struct{ in, want string }{
		{"NAMESPACE  FOLDER  PLAIN  CURRENT  OTHER_KEY  REFERENCE  NO_KEY  FAILED\npersonal  My Box  2  3  0  0  0  1\n  TOTAL  2  3  0  0  0  1\n",
			"plain=2 current=3 other_key=0 reference=0 no_key=0 failed=1"},
		{"ALL USERS  PLAIN  CURRENT  OTHER_KEY  REFERENCE  NO_KEY  FAILED\n150 users  10  20  1  4  0  0\n",
			"plain=10 current=20 other_key=1 reference=4 no_key=0 failed=0"},
		{"error: backend-api unreachable\n", "unread"},
	} {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", fn+"counts")
		cmd.Stdin = strings.NewReader(tc.in)
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != tc.want {
			t.Errorf("counts(%q) = %q, %v; want %q", tc.in, out, err, tc.want)
		}
	}
}

// The keys go only once status --all shows no encrypted message, however the
// run ended; otherwise the stand keeps them with write off. A cleanup that did
// not finish is a FAIL row, so the run cannot end green over it.
func TestTheKeysGoOnlyWhenNoMailIsEncrypted(t *testing.T) {
	src := scenarioSource(t)
	if n := strings.Count(src, `mailcrypt-keys.sh" delete`); n != 1 {
		t.Fatalf("the Secret is deleted at %d places, want one", n)
	}
	finish := regexp.MustCompile(`(?s)\nfinish\(\) \{.*?\n\}\n`).FindString(src)
	for _, want := range []string{
		`elif [ "$(encrypted "$left")" = 0 ]; then
      if ! deploy restore; then
        row FAIL cleanup`,
		`elif ! bash "$HERE/mailcrypt-keys.sh" delete; then
        row FAIL cleanup`,
		`deploy keys-kept -f "$VALUES/values-sandbox-mailcrypt-keys.yaml" ||
        row FAIL cleanup`,
		`row FAIL cleanup "status --all did not answer`,
	} {
		if !strings.Contains(finish, want) {
			t.Errorf("finish() lacks %s", want)
		}
	}
	if !strings.Contains(src, "trap finish EXIT") {
		t.Error("the cleanup is not on EXIT: a failed scenario would leave the keys and the write mode")
	}
}

// The old key is dropped only after the run that needs it finished: a drop
// after a failed --all left thousands of messages no key opened (2026-11-19).
func TestTheOldKeyGoesOnlyAfterAFinishedRun(t *testing.T) {
	src := scenarioSource(t)
	gate := strings.Index(src, `if [ "$code" != 0 ] || [ "$(field "$mid" other_key)" != 0 ] || [ "$(field "$mid" reference)" != 0 ]; then`)
	drop := strings.Index(src, `mailcrypt-keys.sh" rotate-drop`)
	if gate < 0 || drop < 0 || gate > drop {
		t.Fatalf("rotate-drop at %d, its gate at %d: the drop must follow the check", drop, gate)
	}
	if !strings.Contains(src[gate:drop], "exit 1") {
		t.Error("a failed check does not stop the run before the drop")
	}
}

// A fill names its first refusal, and the run's deploys lift the delivery
// limit a fill past a thousand a minute meets, the restore keeping it: a 451
// rate limit once passed for a lost connection (2026-11-19).
func TestAFillSaysWhyAndTheRunLiftsTheLimit(t *testing.T) {
	raw, err := os.ReadFile("fill-mailboxes.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `echo "fill: $user first refusal: $refused" >&2`) {
		t.Error("fill-mailboxes.sh does not name a refusal")
	}
	src := scenarioSource(t)
	for _, want := range []string{
		`RUN_SET=(--set protocol.lmtp.rate_limit.enabled=false)`,
		`case "$name" in restore | keys-kept) set=() ;; esac`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("crypt-scenarios.sh lacks %s", want)
		}
	}
}
